package forge

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neurekadev/git-backup/internal/buildinfo"
	"github.com/neurekadev/git-backup/internal/paths"
)

const (
	productName = "GitBackup"

	// providerMaxAttempts bounds the GET retry loop for transient provider
	// errors.
	providerMaxAttempts = 5

	// providerRetryAfterCap caps the Retry-After honor for provider requests.
	providerRetryAfterCap = 60 * time.Second

	// maxPageBytes bounds one API page read so a hostile response cannot
	// balloon memory.
	maxPageBytes = 256 << 20
)

// The two shared HTTP clients pool TCP/TLS connections across every provider
// client and call. The no-redirect client exists for attachment downloads,
// which must validate every redirect hop themselves.
var (
	sharedHTTPClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     5 * time.Minute,
		},
	}
	sharedNoRedirectClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     5 * time.Minute,
		},
		// Never auto-follow: attachment downloads re-check each hop.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

var userAgentValue = buildUserAgent()

// buildUserAgent renders the product token once. The version comes from the
// GIT_TAG build argument, which is not guaranteed to be a valid HTTP token
// (git tags may contain '/' and arbitrary text) — report the product without a
// version rather than failing every provider request.
func buildUserAgent() string {
	version := buildinfo.LoadFromEnvironment().Version
	if version == buildinfo.FallbackVersion || version == "" || !isHTTPHeaderValue(version) {
		return productName
	}
	return productName + "/" + version
}

// isHTTPHeaderValue accepts the RFC 7230 token character set.
func isHTTPHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", char):
		default:
			return false
		}
	}
	return true
}

// session bundles one provider call's HTTP client and authentication. C#
// equivalents built a client per call; a session is the same idea minus the
// disposal ceremony, since the underlying transport is shared.
type session struct {
	client *http.Client
	scheme string
	token  string
}

func newSession(scheme string, credential *Credential) *session {
	token := ""
	if credential != nil {
		token = strings.TrimSpace(credential.APIKey)
	}
	return &session{client: sharedHTTPClient, scheme: scheme, token: token}
}

// get issues a GET with the session's auth applied, retrying on rate-limit
// (429) and transient server errors (5xx). Rate limits honor the Retry-After
// header when present, otherwise a capped exponential backoff is used. The
// caller owns the returned response.
func (s *session) get(ctx context.Context, requestURL string) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", userAgentValue)
		if s.token != "" {
			request.Header.Set("Authorization", s.scheme+" "+s.token)
		}

		response, err := s.client.Do(request)
		if err != nil {
			return nil, err
		}

		status := response.StatusCode
		if status != http.StatusTooManyRequests && status < 500 {
			return response, nil
		}
		if attempt >= providerMaxAttempts {
			return response, nil
		}

		delay := resolveRetryDelay(response.Header.Get("Retry-After"), attempt, providerRetryAfterCap, true, false)
		drainAndClose(response)

		slogRetryDebug(status, attempt, delay)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func slogRetryDebug(status, attempt int, delay time.Duration) {
	slog.Debug("Retrying provider request.",
		"status", status,
		"attempt", attempt,
		"delaySeconds", strconv.FormatFloat(delay.Seconds(), 'f', 1, 64))
}

// fetchPage reads one JSON-array page. The has-more decision receives the
// response (headers) after the body has been read, and the response is closed
// before returning.
func (s *session) fetchPage(ctx context.Context, requestURL string, page int, hasNext hasNextPageFunc) ([]map[string]any, bool, error) {
	response, err := s.get(ctx, requestURL)
	if err != nil {
		return nil, false, err
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		drainAndClose(response)
		return nil, false, fmt.Errorf("provider request failed with status %d", status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPageBytes))
	if err != nil {
		drainAndClose(response)
		return nil, false, fmt.Errorf("read provider response: %w", err)
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		drainAndClose(response)
		return nil, false, fmt.Errorf("decode provider response: %w", err)
	}

	var items []map[string]any
	if array, ok := decoded.([]any); ok {
		items = make([]map[string]any, 0, len(array))
		for _, element := range array {
			if object, ok := element.(map[string]any); ok {
				items = append(items, object)
			}
		}
	}

	more := hasNext(response, page, len(items))
	drainAndClose(response)
	return items, more, nil
}

// hasNextPageFunc decides whether a paginated endpoint has another page, given
// the response and the number of raw items on the page just read.
type hasNextPageFunc func(response *http.Response, page, itemCount int) bool

// collect walks a paginated JSON-array endpoint to the end and returns the raw
// items.
func (s *session) collect(ctx context.Context, buildURL func(page int) string, hasNext hasNextPageFunc) ([]map[string]any, error) {
	var items []map[string]any
	for page := 1; ; page++ {
		pageItems, more, err := s.fetchPage(ctx, buildURL(page), page, hasNext)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)
		if !more {
			return items, nil
		}
	}
}

// stream walks a paginated endpoint, delivering mapped items page by page so
// the whole result set is never held in memory.
func (s *session) stream(ctx context.Context, buildURL func(page int) string, hasNext hasNextPageFunc, deliver func(map[string]any) error) error {
	for page := 1; ; page++ {
		pageItems, more, err := s.fetchPage(ctx, buildURL(page), page, hasNext)
		if err != nil {
			return err
		}
		for _, item := range pageItems {
			if err := deliver(item); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
	}
}

// collectDiscovered walks a paginated endpoint and maps the raw items into
// discovered repositories, skipping unmappable entries.
func (s *session) collectDiscovered(
	ctx context.Context,
	buildURL func(page int) string,
	hasNext hasNextPageFunc,
	mapItem func(map[string]any) (DiscoveredRepository, bool),
) ([]DiscoveredRepository, error) {
	raw, err := s.collect(ctx, buildURL, hasNext)
	if err != nil {
		return nil, err
	}

	discovered := make([]DiscoveredRepository, 0, len(raw))
	for _, item := range raw {
		if repository, ok := mapItem(item); ok {
			discovered = append(discovered, repository)
		}
	}
	return discovered, nil
}

// parseAbsoluteURL parses a value that must be an absolute URL.
func parseAbsoluteURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !parsed.IsAbs() {
		return nil, fmt.Errorf("invalid URL '%s'", rawURL)
	}
	return parsed, nil
}

// hostOf returns the host of an absolute URL, or an empty string.
func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// queryEscapePath percent-escapes a project path for use as a single URL path
// segment (GitLab's URL-encoded namespace/project form).
func queryEscapePath(path string) string {
	return url.QueryEscape(path)
}

// collectItems lists a paginated collection, maps each raw item once, populates
// the mapped items (their comments and attachments) in parallel up to
// concurrency, and delivers them in page order. Peak memory is one page rather
// than the whole collection, while the per-page fan-out preserves the fetch
// parallelism. Map functions return pointers, so populate can mutate the item
// that is later delivered.
func collectItems[T any](
	ctx context.Context,
	s *session,
	concurrency int,
	buildURL func(page int) string,
	hasNext hasNextPageFunc,
	mapItem func(map[string]any) (T, bool),
	populate func(T) error,
	deliver func(T) error,
) error {
	for page := 1; ; page++ {
		pageItems, more, err := s.fetchPage(ctx, buildURL(page), page, hasNext)
		if err != nil {
			return err
		}

		mapped := make([]T, 0, len(pageItems))
		for _, item := range pageItems {
			if value, ok := mapItem(item); ok {
				mapped = append(mapped, value)
			}
		}

		if err := forEachParallel(ctx, concurrency, mapped, populate); err != nil {
			return err
		}

		for _, value := range mapped {
			if err := deliver(value); err != nil {
				return err
			}
		}

		if !more {
			return nil
		}
	}
}

// forEachParallel runs fn over items with at most concurrency invocations in
// flight, failing on the first error. Callers pass pointer slices when fn must
// mutate its item.
func forEachParallel[T any](ctx context.Context, concurrency int, items []T, fn func(T) error) error {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency == 1 || len(items) <= 1 {
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(item); err != nil {
				return err
			}
		}
		return nil
	}

	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			fail(err)
			break
		}
		if firstErr != nil {
			break
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			fail(ctx.Err())
		}

		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(item); err != nil {
				fail(err)
			}
		}(item)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// pageIsFull is a has-more strategy for offset-paginated APIs that signal
// "more" by returning a full page of pageSize items.
func pageIsFull(pageSize int) hasNextPageFunc {
	return func(_ *http.Response, _, itemCount int) bool {
		return itemCount >= pageSize
	}
}

// gitLabHasNextPage reads GitLab's X-Next-Page response header rather than
// applying a full-page heuristic.
func gitLabHasNextPage(response *http.Response, page, _ int) bool {
	raw := response.Header.Get("X-Next-Page")
	if strings.TrimSpace(raw) == "" {
		return false
	}
	next, err := strconv.Atoi(strings.TrimSpace(raw))
	return err == nil && next > page
}

// mergeDiscoveryWalks runs the discovery walks concurrently and merges their
// results in the order given, keeping the first occurrence of each clone URL.
// Callers list the owned walk first, so a repository that is both owned and
// starred keeps its owned entry — which is what decides whether its issues,
// merge requests, and releases get backed up at all.
func mergeDiscoveryWalks(walks ...func() ([]DiscoveredRepository, error)) ([]DiscoveredRepository, error) {
	results := make([][]DiscoveredRepository, len(walks))
	errs := make([]error, len(walks))

	var wg sync.WaitGroup
	for i, walk := range walks {
		wg.Add(1)
		go func(i int, walk func() ([]DiscoveredRepository, error)) {
			defer wg.Done()
			results[i], errs[i] = walk()
		}(i, walk)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	var merged []DiscoveredRepository
	seen := make(map[string]struct{})
	for _, result := range results {
		for _, repository := range result {
			if _, duplicate := seen[repository.CloneURL]; duplicate {
				continue
			}
			seen[repository.CloneURL] = struct{}{}
			merged = append(merged, repository)
		}
	}
	return merged, nil
}

// distinctByKey keeps the first occurrence of each key (ordinal comparison),
// the first-wins dedupe idiom used across providers.
func distinctByKey[T any](items []T, keySelector func(T) string) []T {
	seen := make(map[string]struct{}, len(items))
	var result []T
	for _, item := range items {
		key := keySelector(item)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func hasAPIKey(credential *Credential) bool {
	return credential != nil && strings.TrimSpace(credential.APIKey) != ""
}

// resolveRetryDelay ports the shared retry-delay computation: honor a
// Retry-After header (seconds delta or HTTP date), clamped to maxDelay only
// when capRetryAfterToMax is set; otherwise a 2^(attempt-1) backoff capped at
// maxDelay, with up to 1s of randomness when jitter is set.
func resolveRetryDelay(header string, attempt int, maxDelay time.Duration, capRetryAfterToMax, jitter bool) time.Duration {
	if header != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
			delta := time.Duration(seconds) * time.Second
			if capRetryAfterToMax && delta > maxDelay {
				return maxDelay
			}
			return delta
		}
		if when, err := http.ParseTime(strings.TrimSpace(header)); err == nil {
			until := time.Until(when)
			if until > 0 {
				if capRetryAfterToMax && until > maxDelay {
					return maxDelay
				}
				return until
			}
		}
	}

	seconds := maxDelay.Seconds()
	if backoff := float64(int64(1) << (attempt - 1)); backoff < seconds {
		seconds = backoff
	}
	backoff := time.Duration(seconds * float64(time.Second))
	if jitter {
		backoff += time.Duration(rand.Int63n(int64(time.Millisecond * 1000)))
	}
	return backoff
}

// --- JSON helpers over map[string]any item trees ---

func jsonString(item map[string]any, key string) (string, bool) {
	value, ok := item[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func jsonInt64(item map[string]any, key string) (int64, bool) {
	value, ok := item[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case string:
		number, err := strconv.ParseInt(typed, 10, 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func jsonBool(item map[string]any, key string) bool {
	value, ok := item[key]
	return ok && value == true
}

// jsonTime parses an ISO-8601 timestamp. Providers always send an offset, but
// offset-less values are assumed UTC, mirroring the previous parser.
func jsonTime(item map[string]any, key string) (*time.Time, bool) {
	text, ok := jsonString(item, key)
	if !ok || text == "" {
		return nil, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return &parsed, true
		}
	}
	return nil, false
}

func jsonNestedString(item map[string]any, objectKey, key string) (string, bool) {
	nested, ok := item[objectKey].(map[string]any)
	if !ok {
		return "", false
	}
	return jsonString(nested, key)
}

// jsonLabels reads a label array, handling both a plain array of strings
// (GitLab) and an array of objects with a name property (GitHub, Forgejo).
func jsonLabels(item map[string]any, key string) []string {
	value, ok := item[key].([]any)
	if !ok {
		return nil
	}

	labels := []string{}
	for _, element := range value {
		var name string
		switch typed := element.(type) {
		case string:
			name = typed
		case map[string]any:
			name, _ = jsonString(typed, "name")
		}
		if strings.TrimSpace(name) != "" {
			labels = append(labels, name)
		}
	}
	return labels
}

// jsonRawID renders an id property that may be a number or a string (GitLab
// snippet ids) into its raw text.
func jsonRawID(item map[string]any, key string) (string, bool) {
	value, ok := item[key]
	if !ok {
		return "", false
	}
	switch typed := value.(type) {
	case float64:
		return strconv.FormatInt(int64(typed), 10), true
	case string:
		return typed, true
	default:
		return "", false
	}
}

// resolveOwnerAndRepository extracts the owner and repository name from a
// clone URL (last path segment as the repository, with any .git suffix
// removed). Used by providers whose metadata endpoints address a project as
// /repos/{owner}/{repo}.
func resolveOwnerAndRepository(cloneURL string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || !parsed.IsAbs() {
		return "", "", fmt.Errorf("invalid repository URL '%s'.", cloneURL)
	}

	segments := paths.SplitUnescapedSegments(parsed)
	if len(segments) < 2 {
		return "", "", fmt.Errorf("repository URL '%s' does not contain owner and repository segments.", cloneURL)
	}

	repository := paths.TrimGitSuffix(segments[len(segments)-1])
	return segments[0], repository, nil
}

// buildOwnerRepoPath builds the URL-escaped {owner}/{repo} path segment for
// providers that address a project as /repos/{owner}/{repo}.
func buildOwnerRepoPath(cloneURL string) (string, error) {
	owner, repository, err := resolveOwnerAndRepository(cloneURL)
	if err != nil {
		return "", err
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(repository), nil
}

// resolveInstanceHost is the host of the forge a repository came from, or an
// empty string when it cannot be determined.
func resolveInstanceHost(context *MetadataContext) string {
	reference := context.WebURL
	if reference == "" {
		reference = context.CloneURL
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// ShortHash is a short, stable hex prefix derived from a value (e.g. an
// attachment URL), used to keep storage keys unique and idempotent across runs
// when the provider gives no natural content hash.
func ShortHash(value string) string {
	hash := sha1.Sum([]byte(value))
	return hex.EncodeToString(hash[:4])
}
