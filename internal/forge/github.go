package forge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GitHubClient discovers repositories and fetches metadata from GitHub's REST
// API (api.github.com, or /api/v3 for self-hosted installations).
type GitHubClient struct{}

// NewGitHubClient returns the GitHub provider client.
func NewGitHubClient() *GitHubClient { return &GitHubClient{} }

const (
	githubDefaultAPIBaseURL = "https://api.github.com"
	githubPageSize          = 100
	githubAuthScheme        = "Bearer"
)

// githubAttachmentReference matches GitHub's attachment URLs embedded in
// markdown bodies: GitHub stores issue/PR attachments as absolute URLs on its
// attachment hosts.
var githubAttachmentReference = regexp.MustCompile(
	`(?i)https?://(?:user-images\.githubusercontent\.com|private-user-images\.githubusercontent\.com|github\.com/user-attachments/(?:assets|files)|github\.com/[^/\s)]+/[^/\s)]+/(?:assets|files))/[^\s)\]"'<>]+`)

func (c *GitHubClient) Provider() string            { return "github" }
func (c *GitHubClient) SupportsSnippets() bool      { return true }
func (c *GitHubClient) SupportsIssues() bool        { return true }
func (c *GitHubClient) SupportsMergeRequests() bool { return true }
func (c *GitHubClient) SupportsReleases() bool      { return true }
func (c *GitHubClient) SupportsArtifacts() bool     { return true }

// ListRepositories walks the owned (and optionally starred, gist) endpoints
// concurrently and merges the results owned-first.
func (c *GitHubClient) ListRepositories(ctx context.Context, repository RepositoryJobOptions, credential *Credential) ([]DiscoveredRepository, error) {
	if !hasAPIKey(credential) {
		return nil, nil
	}

	baseURL := resolveGitHubAPIBase(repository.BaseURL)
	s := newSession(githubAuthScheme, credential)

	walks := []func() ([]DiscoveredRepository, error){
		func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/user/repos?affiliation=owner&visibility=all&per_page=%d&page=%d", baseURL, githubPageSize, page)
			}, pageIsFull(githubPageSize), func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGiteaRepository(item, false)
			})
		},
	}

	if repository.IncludeStarred {
		slog.Debug("Including starred repositories.", "provider", c.Provider())
		walks = append(walks, func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/user/starred?per_page=%d&page=%d", baseURL, githubPageSize, page)
			}, pageIsFull(githubPageSize), func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGiteaRepository(item, true)
			})
		})
	}

	if repository.IncludeSnippets {
		slog.Debug("Including gists.", "provider", c.Provider())
		walks = append(walks, func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/gists?per_page=%d&page=%d", baseURL, githubPageSize, page)
			}, pageIsFull(githubPageSize), mapGist)
		})

		if repository.IncludeStarred {
			slog.Debug("Including starred gists.", "provider", c.Provider())
			walks = append(walks, func() ([]DiscoveredRepository, error) {
				return s.collectDiscovered(ctx, func(page int) string {
					return fmt.Sprintf("%s/gists/starred?per_page=%d&page=%d", baseURL, githubPageSize, page)
				}, pageIsFull(githubPageSize), mapGist)
			})
		}
	}

	return mergeDiscoveryWalks(walks...)
}

// ListIssues streams the repository's issues (skipping pull-request entries),
// attaching the repo-wide grouped comments and body-scanned attachments.
func (c *GitHubClient) ListIssues(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Issue) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitHubAPIBase(context.BaseURL)
	s := newSession(githubAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	commentsByNumber, err := c.issueCommentsByNumber(ctx, context, s, baseURL, repositoryPath)
	if err != nil {
		return err
	}

	return s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=%d&page=%d", baseURL, repositoryPath, githubPageSize, page)
	}, pageIsFull(githubPageSize), func(item map[string]any) error {
		issue, ok := mapGitHubIssue(item)
		if !ok {
			return nil
		}
		issue.Comments = commentsByNumber[issue.Number]
		issue.Attachments = extractGitHubAttachments(issue.Body, issue.Comments)
		return deliver(*issue)
	})
}

// ListMergeRequests streams pull requests. Pull requests are issues on
// GitHub, so their discussion comments live on the same repo-wide
// issue-comments endpoint the issue pass already walked; the walk result is
// reused rather than paged a second time.
func (c *GitHubClient) ListMergeRequests(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(MergeRequest) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitHubAPIBase(context.BaseURL)
	s := newSession(githubAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	commentsByNumber, err := c.issueCommentsByNumber(ctx, context, s, baseURL, repositoryPath)
	if err != nil {
		return err
	}

	return s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/pulls?state=all&per_page=%d&page=%d", baseURL, repositoryPath, githubPageSize, page)
	}, pageIsFull(githubPageSize), func(item map[string]any) error {
		pull, ok := mapGiteaPullRequest(item)
		if !ok {
			return nil
		}
		pull.Comments = commentsByNumber[pull.Number]
		pull.Attachments = extractGitHubAttachments(pull.Body, pull.Comments)
		return deliver(*pull)
	})
}

func (c *GitHubClient) ListReleases(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Release) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitHubAPIBase(context.BaseURL)
	s := newSession(githubAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	return s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", baseURL, repositoryPath, githubPageSize, page)
	}, pageIsFull(githubPageSize), func(item map[string]any) error {
		release, ok := mapGiteaRelease(item)
		if !ok {
			return nil
		}
		return deliver(*release)
	})
}

// OpenAttachment opens a GitHub-hosted attachment stream.
func (c *GitHubClient) OpenAttachment(ctx context.Context, context *MetadataContext, credential *Credential, downloadURL string) (*AttachmentStream, error) {
	s := newSession(githubAuthScheme, credential)
	return openAttachmentStream(ctx, s, downloadURL, resolveInstanceHost(context))
}

// commentCache holds the repo-wide comment walk shared by the issue and
// pull-request passes; computed once per repository context.
type commentCache struct {
	once     sync.Once
	byNumber map[int64][]Comment
	err      error
}

// issueCommentsByNumber returns the repository's comments grouped by
// issue/PR number, walking the endpoint at most once per repository. The issue
// and pull-request passes both need the same repo-wide data and run one after
// the other, so the result is cached on the per-repository context.
func (c *GitHubClient) issueCommentsByNumber(ctx context.Context, context *MetadataContext, s *session, baseURL, repositoryPath string) (map[int64][]Comment, error) {
	cache := context.commentCacheFor()
	cache.once.Do(func() {
		cache.byNumber, cache.err = fetchIssueCommentsByNumber(ctx, s, baseURL, repositoryPath)
	})
	return cache.byNumber, cache.err
}

// fetchIssueCommentsByNumber fetches every issue and PR comment for the repo
// in one paginated walk and groups them by the issue/PR number parsed from
// each comment's issue_url. Requested oldest-first so each thread stays in
// chronological order.
//
// GitHub has no per-page comment endpoint, so this materializes the
// repository's whole comment corpus up front: peak memory here scales with the
// total comment count rather than with one page. That is the deliberate trade
// for removing the per-item N+1 (one request per issue/PR), and it is released
// once the repository's context goes out of scope.
func fetchIssueCommentsByNumber(ctx context.Context, s *session, baseURL, repositoryPath string) (map[int64][]Comment, error) {
	commentsByNumber := make(map[int64][]Comment)

	err := s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/issues/comments?sort=created&direction=asc&per_page=%d&page=%d", baseURL, repositoryPath, githubPageSize, page)
	}, pageIsFull(githubPageSize), func(item map[string]any) error {
		comment, ok := mapGiteaComment(item)
		if !ok {
			return nil
		}
		number, ok := parseIssueNumberFromURL(item)
		if !ok {
			return nil
		}
		commentsByNumber[number] = append(commentsByNumber[number], comment)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return commentsByNumber, nil
}

// parseIssueNumberFromURL extracts the trailing issue/PR number from a GitHub
// issue_url such as https://api.github.com/repos/{owner}/{repo}/issues/{number}.
func parseIssueNumberFromURL(item map[string]any) (int64, bool) {
	issueURL, _ := jsonString(item, "issue_url")
	if issueURL == "" {
		return 0, false
	}
	segment := issueURL
	if lastSlash := strings.LastIndex(issueURL, "/"); lastSlash >= 0 {
		segment = issueURL[lastSlash+1:]
	}
	number, err := strconv.ParseInt(segment, 10, 64)
	return number, err == nil
}

// mapGitHubIssue maps an issue, skipping pull requests: the GitHub issues
// endpoint also returns pull requests, which carry a pull_request object and
// are backed up via the pulls endpoint instead.
func mapGitHubIssue(item map[string]any) (*Issue, bool) {
	if _, hasPullRequest := item["pull_request"]; hasPullRequest {
		return nil, false
	}
	return mapGiteaIssue(item)
}

func mapGist(item map[string]any) (DiscoveredRepository, bool) {
	cloneURL, _ := jsonString(item, "git_pull_url")
	id, _ := jsonString(item, "id")
	if cloneURL == "" || id == "" {
		return DiscoveredRepository{}, false
	}

	webURL, _ := jsonString(item, "html_url")
	return DiscoveredRepository{
		CloneURL:   cloneURL,
		WebURL:     webURL,
		Kind:       KindGist,
		Identifier: id,
	}, true
}

// extractGitHubAttachments scans an issue/PR body and its comments for
// GitHub attachment URLs.
func extractGitHubAttachments(body *string, comments []Comment) []Attachment {
	return scanBodyAndComments(body, comments, githubAttachmentReference, func(match string) (Attachment, bool) {
		// The trailing pattern permits '/' and '.', so untrusted body text can
		// embed dot-segments that URL normalization would resolve into a
		// different path on the same allowlisted host before the request goes
		// out with the token attached. Match on whole segments so a name like
		// "chart..v2.png" is still accepted.
		pathOnly := strings.SplitN(match, "?", 2)[0]
		for _, segment := range strings.Split(pathOnly, "/") {
			if segment == ".." {
				return Attachment{}, false
			}
		}

		// Persist and key off the query-free URL so the short-lived ?jwt=
		// signing token is never stored (and the key stays stable across
		// runs); the full URL is kept only for download.
		reference := RedactURL(match)
		return Attachment{
			FileName:     BuildStorageFileName(reference, lastPathSegment(reference)),
			OriginalPath: reference,
			DownloadURL:  match,
		}, true
	})
}

func lastPathSegment(rawURL string) string {
	withoutQuery := strings.SplitN(rawURL, "?", 2)[0]
	if lastSlash := strings.LastIndex(withoutQuery, "/"); lastSlash >= 0 {
		return withoutQuery[lastSlash+1:]
	}
	return withoutQuery
}

// resolveGitHubAPIBase resolves the API base from an optional configured base
// URL: the default api.github.com, the configured value when it already
// contains /api/, or the configured value + /api/v3 for self-hosted
// installations.
func resolveGitHubAPIBase(configuredBaseURL string) string {
	if configuredBaseURL == "" {
		return githubDefaultAPIBaseURL
	}

	trimmed := resolveBaseURL(configuredBaseURL, githubDefaultAPIBaseURL)
	if strings.Contains(strings.ToLower(trimmed), "/api/") {
		return trimmed
	}
	if strings.EqualFold(trimmed, "https://github.com") {
		return githubDefaultAPIBaseURL
	}
	return trimmed + "/api/v3"
}

// resolveBaseURL cleans a configured base URL or falls back to the provider
// default.
func resolveBaseURL(configuredBaseURL, defaultBaseURL string) string {
	if configuredBaseURL == "" {
		return strings.TrimSuffix(defaultBaseURL, "/")
	}
	return strings.TrimSuffix(strings.TrimSpace(configuredBaseURL), "/")
}

// ensureAPISuffix appends the provider's API path suffix unless it is present
// already.
func ensureAPISuffix(baseURL, apiSuffix string) string {
	if strings.HasSuffix(strings.ToLower(baseURL), strings.ToLower(apiSuffix)) {
		return baseURL
	}
	return baseURL + apiSuffix
}

// composeAPIBaseURL resolves a provider API base URL from an optional
// configured value, a provider default, and the provider's API path suffix
// (e.g. /api/v4).
func composeAPIBaseURL(configuredBaseURL, defaultBaseURL, apiSuffix string) string {
	return ensureAPISuffix(resolveBaseURL(configuredBaseURL, defaultBaseURL), apiSuffix)
}

// scanBodyAndComments scans an item body and its comment bodies for
// attachment references matching pattern, builds each match via build, and
// dedupes by OriginalPath (first wins). Shared by providers that embed
// attachments inline in markdown (GitHub, GitLab). build returns false for a
// match the provider decides not to trust (e.g. an upload reference carrying a
// path separator); those are skipped rather than recorded.
func scanBodyAndComments(body *string, comments []Comment, pattern *regexp.Regexp, build func(match string) (Attachment, bool)) []Attachment {
	seen := make(map[string]struct{})
	var attachments []Attachment

	scan := func(text *string) {
		if text == nil || *text == "" {
			return
		}
		for _, match := range pattern.FindAllString(*text, -1) {
			attachment, ok := build(match)
			if !ok {
				continue
			}
			if _, duplicate := seen[attachment.OriginalPath]; duplicate {
				continue
			}
			seen[attachment.OriginalPath] = struct{}{}
			attachments = append(attachments, attachment)
		}
	}

	scan(body)
	for _, comment := range comments {
		scan(comment.Body)
	}
	return attachments
}
