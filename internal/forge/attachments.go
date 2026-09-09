package forge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/neurekadev/git-backup/internal/paths"
)

// Attachment download helpers: attachments are streamed straight through to
// storage rather than buffered in memory, a size cap guards against
// pathological uploads, and every redirect hop is SSRF-checked before it is
// followed.

const (
	// MaxAttachmentBytes caps one attachment download.
	MaxAttachmentBytes = 100 << 20

	maxRedirects = 5
)

// AttachmentStream is a size-capped attachment body. The caller uploads it
// straight to storage and then Closes it, releasing the HTTP response. When
// the server declared a Content-Length it is exposed via KnownLength so the
// storage layer can pick a single-request upload for a small attachment;
// otherwise KnownLength is negative.
type AttachmentStream struct {
	body        io.ReadCloser
	maxBytes    int64
	totalRead   int64
	KnownLength int64
}

// NewAttachmentStream wraps a body as a size-capped attachment stream. It is
// exported so embedders (and tests) can feed streams through the same upload
// path the providers use.
func NewAttachmentStream(body io.ReadCloser, maxBytes, knownLength int64) *AttachmentStream {
	return &AttachmentStream{body: body, maxBytes: maxBytes, KnownLength: knownLength}
}

func (a *AttachmentStream) Read(p []byte) (int, error) {
	n, err := a.body.Read(p)
	if n > 0 {
		a.totalRead += int64(n)
		if a.totalRead > a.maxBytes {
			return n, fmt.Errorf("attachment exceeds the %d byte limit", a.maxBytes)
		}
	}
	return n, err
}

func (a *AttachmentStream) Close() error {
	return a.body.Close()
}

// openAttachmentStream opens an attachment as a streaming, size-capped read.
// Redirects are followed manually so every hop is re-checked by
// ensureSafeDownloadHost (a plain auto-redirect would validate only the first
// URL). The auth header is sent only while the request stays on the original
// host — a redirect to any other host drops it, so the token never reaches a
// redirect target.
func openAttachmentStream(ctx context.Context, s *session, downloadURL, trustedHost string) (*AttachmentStream, error) {
	current, err := url.Parse(downloadURL)
	if err != nil || !current.IsAbs() {
		return nil, fmt.Errorf("attachment URL '%s' is not a valid absolute URL.", RedactURL(downloadURL))
	}

	originalHost := current.Hostname()

	var response *http.Response
	for hop := 0; ; hop++ {
		if err := ensureSafeDownloadHost(ctx, current, trustedHost); err != nil {
			return nil, err
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", userAgentValue)
		if s.token != "" && strings.EqualFold(current.Hostname(), originalHost) {
			request.Header.Set("Authorization", s.scheme+" "+s.token)
		}

		response, err = sharedNoRedirectClient.Do(request)
		if err != nil {
			return nil, err
		}

		if !isRedirectStatus(response.StatusCode) || response.Header.Get("Location") == "" || hop >= maxRedirects {
			break
		}

		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			drainAndClose(response)
			return nil, fmt.Errorf("attachment redirect target is invalid: %w", err)
		}
		next := response.Request.URL.ResolveReference(location)
		drainAndClose(response)
		current = next
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		drainAndClose(response)
		return nil, fmt.Errorf("attachment download failed with status %d", status)
	}

	declaredLength := response.ContentLength
	if declaredLength > MaxAttachmentBytes {
		drainAndClose(response)
		return nil, fmt.Errorf("attachment '%s' is %d bytes, over the %d byte limit.",
			RedactURL(downloadURL), declaredLength, MaxAttachmentBytes)
	}

	knownLength := int64(-1)
	if declaredLength >= 0 {
		knownLength = declaredLength
	}
	return &AttachmentStream{
		body:        response.Body,
		maxBytes:    MaxAttachmentBytes,
		KnownLength: knownLength,
	}, nil
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// ensureSafeDownloadHost rejects an attachment URL that is not http(s) or that
// resolves to a private, loopback, or link-local address, so a crafted
// provider response (or a redirect hop) cannot make the authenticated client
// reach an internal endpoint (e.g. a cloud metadata service). trustedHost is
// the forge this repository came from, which is exempt: a self-hosted instance
// is routinely on private address space and is already trusted, since the API
// calls that discovered the attachment went to that same host with the same
// credential. Every other target still fails closed. DNS names are resolved
// and every returned address is checked, not just literal-IP hosts. A residual
// DNS-rebinding TOCTOU remains (the name could resolve differently when the
// socket actually connects); fully closing it would require pinning the
// connection to the validated address.
func ensureSafeDownloadHost(ctx context.Context, uri *url.URL, trustedHost string) error {
	if !paths.IsHTTPOrHTTPS(uri) {
		return fmt.Errorf("attachment URL '%s' is not an http or https URL.", RedactURL(uri.String()))
	}

	if trustedHost != "" && strings.EqualFold(uri.Hostname(), trustedHost) {
		return nil
	}

	host := uri.Hostname()
	var addresses []net.IP
	if literal := net.ParseIP(host); literal != nil {
		addresses = []net.IP{literal}
	} else {
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return fmt.Errorf("attachment host '%s' could not be resolved: %w", host, err)
		}
		if len(resolved) == 0 {
			return fmt.Errorf("attachment host '%s' did not resolve to any address.", host)
		}
		addresses = make([]net.IP, 0, len(resolved))
		for _, addr := range resolved {
			addresses = append(addresses, addr.IP)
		}
	}

	for _, address := range addresses {
		if isPrivateOrLocal(address) {
			return fmt.Errorf("attachment URL '%s' resolves to a private, loopback, or link-local address.", RedactURL(uri.String()))
		}
	}
	return nil
}

func isPrivateOrLocal(address net.IP) bool {
	if address.IsLoopback() {
		return true
	}

	if ipv4 := address.To4(); ipv4 != nil {
		return ipv4[0] == 0 || ipv4[0] == 10 ||
			(ipv4[0] == 172 && ipv4[1] >= 16 && ipv4[1] <= 31) ||
			(ipv4[0] == 192 && ipv4[1] == 168) ||
			(ipv4[0] == 169 && ipv4[1] == 254)
	}

	ipv6 := address.To16()
	if ipv6 == nil {
		return false
	}
	return address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		isIPv6SiteLocal(ipv6) ||
		// fc00::/7 unique local addresses.
		ipv6[0]&0xFE == 0xFC
}

// isIPv6SiteLocal reports whether the address is in fec0::/10, the deprecated
// site-local range (Go has no direct predicate for it).
func isIPv6SiteLocal(ipv6 net.IP) bool {
	return ipv6[0] == 0xFE && ipv6[1]&0xC0 == 0x80
}

// SanitizeFileName produces a safe storage-key leaf from an upload's raw file
// name: strips any path, decodes URL escapes, and replaces characters outside
// [A-Za-z0-9._-] so the name matches the same normalization discipline used
// for repository path segments.
func SanitizeFileName(fileName string) string {
	candidate := strings.TrimSpace(fileName)

	lastSeparator := strings.LastIndexAny(candidate, `/\`)
	if lastSeparator >= 0 {
		candidate = candidate[lastSeparator+1:]
	}

	unescaped, err := url.PathUnescape(candidate)
	if err != nil {
		unescaped = candidate
	}
	return paths.NormalizeStorageSegment(unescaped, "file", false)
}

// RedactURL strips the query string from a URL so short-lived signed tokens
// (e.g. GitHub's private-user-images…?jwt=) are never written to a log or
// persisted as a stored attachment reference. Using the query-free form also
// keeps the derived storage key stable across runs, since the signing token
// changes on every API read.
func RedactURL(rawURL string) string {
	if index := strings.Index(rawURL, "?"); index >= 0 {
		return rawURL[:index]
	}
	return rawURL
}

// BuildStorageFileName builds a collision-resistant storage-key leaf as
// {ShortHash(hashSeed)}-{SanitizeFileName(rawName)} — the shared naming
// convention for downloaded attachments and release assets.
func BuildStorageFileName(hashSeed, rawName string) string {
	return ShortHash(hashSeed) + "-" + SanitizeFileName(rawName)
}

// drainAndClose discards a bounded amount of a response body and closes it so
// the underlying connection can be reused.
func drainAndClose(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	_ = response.Body.Close()
}
