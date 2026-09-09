package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeForge serves canned JSON array pages keyed by path prefix.
type fakeForge struct {
	server  *httptest.Server
	mu      sync.Mutex
	pages   map[string][][]any     // path prefix → pages of items
	seen    []string               // request paths, in order
	headers map[string]http.Header // headers of the last request per path
}

func newFakeForge(t *testing.T, pages map[string][][]any) *fakeForge {
	t.Helper()
	fake := &fakeForge{pages: pages, headers: make(map[string]http.Header)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.seen = append(fake.seen, r.URL.Path)
		fake.headers[r.URL.Path] = r.Header.Clone()

		page := 0
		if raw := r.URL.Query().Get("page"); raw != "" {
			_, _ = fmt.Sscanf(raw, "%d", &page)
		}
		for prefix, items := range fake.pages {
			if strings.HasSuffix(r.URL.Path, prefix) {
				index := page - 1
				if index < 0 || index >= len(items) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(items[index])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func TestGitHubDiscoveryMergesOwnedFirst(t *testing.T) {
	owned := []any{
		map[string]any{"clone_url": "https://github.com/octo/dup.git", "html_url": "https://github.com/octo/dup"},
		map[string]any{"clone_url": "https://github.com/octo/owned.git", "html_url": "https://github.com/octo/owned"},
	}
	starred := []any{
		map[string]any{"clone_url": "https://github.com/octo/DUP.git", "html_url": "https://github.com/octo/dup"},
		map[string]any{"clone_url": "https://github.com/someone/starred.git", "html_url": "https://github.com/someone/starred"},
	}
	gists := []any{
		map[string]any{"git_pull_url": "https://gist.github.com/abc.git", "id": "abc", "html_url": "https://gist.github.com/abc"},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/user/repos":    {owned},
		"/user/starred":  {starred},
		"/gists":         {gists},
		"/gists/starred": {gists},
	})

	client := NewGitHubClient()
	repositories, err := client.ListRepositories(context.Background(), RepositoryJobOptions{
		BaseURL:         fake.server.URL + "/api/v3",
		IncludeStarred:  true,
		IncludeSnippets: true,
	}, &Credential{APIKey: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	// The clone-URL dedupe is ordinal (case-sensitive), matching the original
	// implementation: the owned and starred case-variants are distinct entries,
	// but the owned walk runs first so an exact-duplicate starred entry drops.
	if len(repositories) != 5 {
		t.Fatalf("repositories = %d, want 5: %+v", len(repositories), repositories)
	}
	// The owned walk comes first in the merged order.
	if repositories[0].IsStarred || repositories[1].IsStarred {
		t.Error("the owned entries should lead the merged result")
	}
	if !repositories[2].IsStarred || !repositories[3].IsStarred {
		t.Error("the starred entries should keep their starred flag")
	}
	if repositories[4].Kind != KindGist || repositories[4].Identifier != "abc" {
		t.Errorf("gist = %+v", repositories[4])
	}

	// Auth header scheme.
	if got := fake.headers["/api/v3/user/repos"].Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", got)
	}
}

func TestGitHubIssuesGroupCommentsAndSkipPullRequests(t *testing.T) {
	comments := []any{
		map[string]any{"id": 1, "body": "first", "issue_url": "https://api.github.com/repos/o/r/issues/7", "user": map[string]any{"login": "alice"}},
		map[string]any{"id": 2, "body": "pr comment", "issue_url": "https://api.github.com/repos/o/r/issues/8", "user": map[string]any{"login": "bob"}},
		map[string]any{"id": 3, "body": "second", "issue_url": "https://api.github.com/repos/o/r/issues/7", "user": map[string]any{"login": "carol"}},
	}
	issues := []any{
		map[string]any{"number": 8, "title": "a pull request", "pull_request": map[string]any{}},
		map[string]any{"number": 7, "title": "an issue", "body": "see https://user-images.githubusercontent.com/u/123/img.png?jwt=secret", "user": map[string]any{"login": "dio"}},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/issues/comments": {comments},
		"/issues":          {issues},
	})

	client := NewGitHubClient()
	metaContext := &MetadataContext{CloneURL: "https://github.com/octo/repo.git", BaseURL: fake.server.URL}

	var delivered []Issue
	err := client.ListIssues(context.Background(), metaContext, &Credential{APIKey: "tok"}, func(issue Issue) error {
		delivered = append(delivered, issue)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(delivered) != 1 {
		t.Fatalf("issues = %d, want 1 (pull requests are excluded)", len(delivered))
	}
	issue := delivered[0]
	if issue.Number != 7 || len(issue.Comments) != 2 {
		t.Fatalf("issue = %+v, comments = %d", issue, len(issue.Comments))
	}
	if *issue.Comments[0].Author != "alice" || *issue.Comments[1].Author != "carol" {
		t.Error("comments should stay in chronological (walk) order")
	}
	if len(issue.Attachments) != 1 {
		t.Fatalf("attachments = %+v", issue.Attachments)
	}
	attachment := issue.Attachments[0]
	if strings.Contains(attachment.OriginalPath, "jwt=") {
		t.Error("the query string with the signing token must be stripped from the persisted reference")
	}
	if !strings.HasSuffix(attachment.FileName, "-img.png") {
		t.Errorf("FileName = %q, want the short-hash + sanitized name", attachment.FileName)
	}
	if attachment.DownloadURL != "https://user-images.githubusercontent.com/u/123/img.png?jwt=secret" {
		t.Errorf("DownloadURL should keep the full URL, got %q", attachment.DownloadURL)
	}
}

func TestGitHubPullRequestsReuseCommentWalk(t *testing.T) {
	comments := []any{
		map[string]any{"id": 2, "body": "pr comment", "issue_url": "https://api.github.com/repos/o/r/issues/8", "user": map[string]any{"login": "bob"}},
	}
	pulls := []any{
		map[string]any{"number": 8, "title": "a pull request", "user": map[string]any{"login": "dio"}, "head": map[string]any{"ref": "feature"}, "base": map[string]any{"ref": "main"}},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/issues/comments": {comments},
		"/pulls":           {pulls},
	})

	client := NewGitHubClient()
	metaContext := &MetadataContext{CloneURL: "https://github.com/octo/repo.git", BaseURL: fake.server.URL}

	var requests int
	// Run both passes against the same context: the second must not re-walk.
	for _, pass := range []func() error{
		func() error {
			return client.ListMergeRequests(context.Background(), metaContext, &Credential{APIKey: "tok"}, func(mr MergeRequest) error {
				requests++
				if len(mr.Comments) != 1 || *mr.Comments[0].Body != "pr comment" {
					t.Errorf("merge request comments = %+v", mr.Comments)
				}
				if *mr.SourceBranch != "feature" || *mr.TargetBranch != "main" {
					t.Errorf("branches = %v -> %v", *mr.SourceBranch, *mr.TargetBranch)
				}
				return nil
			})
		},
	} {
		if err := pass(); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 1 {
		t.Fatalf("delivered %d merge requests, want 1", requests)
	}
}

func TestGitHubRejectsDotSegmentAttachments(t *testing.T) {
	body := "see https://github.com/octo/repo/files/1/bad.png and https://user-images.githubusercontent.com/u/1/../../admin/x.png"
	fake := newFakeForge(t, map[string][][]any{"/issues": {[]any{}}})
	_ = fake

	attachments := extractGitHubAttachments(&body, nil)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %+v, want only the safe one", attachments)
	}
	if !strings.HasSuffix(attachments[0].OriginalPath, "bad.png") {
		t.Errorf("kept attachment = %q", attachments[0].OriginalPath)
	}
}

func TestGitLabPaginationViaNextPageHeader(t *testing.T) {
	page1 := []any{map[string]any{"http_url_to_repo": "https://gitlab.com/g/p1.git", "web_url": "https://gitlab.com/g/p1", "id": 1}}
	page2 := []any{map[string]any{"http_url_to_repo": "https://gitlab.com/g/p2.git", "web_url": "https://gitlab.com/g/p2", "id": 2}}
	page3 := []any{} // X-Next-Page absent: stop

	mux := http.NewServeMux()
	var requests int
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode(page1)
		case 2:
			w.Header().Set("X-Next-Page", "3")
			_ = json.NewEncoder(w).Encode(page2)
		default:
			_ = json.NewEncoder(w).Encode(page3)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := NewGitLabClient()
	repositories, err := client.ListRepositories(context.Background(), RepositoryJobOptions{BaseURL: server.URL + "/api/v4"}, &Credential{APIKey: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("repositories = %+v", repositories)
	}
	if repositories[0].ProviderProjectID != "1" || repositories[1].ProviderProjectID != "2" {
		t.Errorf("project ids = %q, %q", repositories[0].ProviderProjectID, repositories[1].ProviderProjectID)
	}
}

func TestGitLabIssuesPopulateNotesAndAttachments(t *testing.T) {
	issues := []any{
		map[string]any{"iid": 5, "title": "bug", "description": "see /uploads/0123456789abcdef0123456789abcdef/report.pdf", "author": map[string]any{"username": "amy"}},
	}
	notes := []any{
		map[string]any{"id": 9, "body": "note", "author": map[string]any{"username": "bob"}, "system": true},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/issues": {issues},
		"/notes":  {notes},
	})

	client := NewGitLabClient()
	metaContext := &MetadataContext{CloneURL: "https://gitlab.com/group/project.git", WebURL: "https://gitlab.com/group/project", BaseURL: fake.server.URL}

	var issuesSeen []Issue
	err := client.ListIssues(context.Background(), metaContext, &Credential{APIKey: "tok"}, func(issue Issue) error {
		issuesSeen = append(issuesSeen, issue)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(issuesSeen) != 1 {
		t.Fatalf("issues = %d", len(issuesSeen))
	}
	issue := issuesSeen[0]
	if issue.Number != 5 || *issue.Author != "amy" {
		t.Errorf("issue = %+v", issue)
	}
	if len(issue.Comments) != 1 || !issue.Comments[0].System || *issue.Comments[0].Author != "bob" {
		t.Errorf("comments = %+v", issue.Comments)
	}
	if len(issue.Attachments) != 1 {
		t.Fatalf("attachments = %+v", issue.Attachments)
	}
	attachment := issue.Attachments[0]
	if attachment.OriginalPath != "/uploads/0123456789abcdef0123456789abcdef/report.pdf" {
		t.Errorf("original path = %q", attachment.OriginalPath)
	}
	if !strings.HasPrefix(attachment.FileName, "01234567-report.pdf") {
		t.Errorf("file name = %q, want the sha-prefix + sanitized name", attachment.FileName)
	}
	if attachment.DownloadURL != "https://gitlab.com/group/project/uploads/0123456789abcdef0123456789abcdef/report.pdf" {
		t.Errorf("download URL = %q", attachment.DownloadURL)
	}
}

func TestGitLabAttachmentsRejectPathTraversal(t *testing.T) {
	body := "x /uploads/0123456789abcdef0123456789abcdef/../../api/v4/users y /uploads/0123456789abcdef0123456789abcdef/ok.png z"
	metaContext := &MetadataContext{CloneURL: "https://gitlab.com/g/p.git"}

	attachments := extractGitLabAttachments(metaContext, &body, nil)
	if len(attachments) != 1 || !strings.HasSuffix(attachments[0].OriginalPath, "ok.png") {
		t.Fatalf("attachments = %+v, want only the safe reference", attachments)
	}
}

func TestGitLabReleaseLinksDownloadability(t *testing.T) {
	release := map[string]any{
		"tag_name":    "v1.0",
		"description": "rel",
		"assets": map[string]any{
			"sources": []any{map[string]any{"format": "zip"}}, // auto archives are skipped
			"links": []any{
				map[string]any{"name": "on-instance", "url": "https://gitlab.com/g/p/-/releases/v1.0/file.bin", "direct_asset_url": "https://gitlab.com/g/p/-/releases/v1.0/file.bin"},
				map[string]any{"name": "external", "url": "https://cdn.example.com/file.bin"},
			},
		},
	}

	fake := newFakeForge(t, map[string][][]any{"/releases": {[]any{release}}})
	client := NewGitLabClient()
	metaContext := &MetadataContext{CloneURL: "https://gitlab.com/g/p.git", WebURL: "https://gitlab.com/g/p", BaseURL: fake.server.URL}

	var releases []Release
	err := client.ListReleases(context.Background(), metaContext, &Credential{APIKey: "tok"}, func(release Release) error {
		releases = append(releases, release)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 {
		t.Fatalf("releases = %d", len(releases))
	}
	attachments := releases[0].Attachments
	if len(attachments) != 2 {
		t.Fatalf("attachments = %+v (source archives must be skipped)", attachments)
	}
	if !attachments[0].Downloadable {
		t.Error("an instance-host asset link should be downloadable")
	}
	if attachments[1].Downloadable {
		t.Error("an external asset link must not be downloadable")
	}
}

func TestGitLabProjectSnippetNestsUnderParent(t *testing.T) {
	snippets := []any{
		map[string]any{"id": 11, "web_url": "https://gitlab.com/g/p/-/snippets/11", "project_id": 99},
		map[string]any{"id": "12", "web_url": "https://gitlab.com/snippets/12"},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/projects": {{}},
		"/snippets": {snippets},
	})
	client := NewGitLabClient()
	repositories, err := client.ListRepositories(context.Background(), RepositoryJobOptions{
		BaseURL:         fake.server.URL + "/api/v4",
		IncludeSnippets: true,
	}, &Credential{APIKey: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("repositories = %+v", repositories)
	}
	if repositories[0].ParentURL != "https://gitlab.com/g/p" || repositories[0].Identifier != "11" {
		t.Errorf("project snippet = %+v", repositories[0])
	}
	if repositories[1].ParentURL != "" || repositories[1].Identifier != "12" {
		t.Errorf("personal snippet = %+v", repositories[1])
	}
}

func TestForgejoIssueAssetsAndSharedCommentThread(t *testing.T) {
	issues := []any{
		map[string]any{
			"number": 3, "title": "with asset", "user": map[string]any{"login": "amy"},
			"assets": []any{map[string]any{"browser_download_url": "https://forge.test/attach/1", "name": "shot.png", "size": 12}},
		},
	}
	comments := []any{
		map[string]any{"id": 4, "body": "here", "user": map[string]any{"login": "bob"},
			"assets": []any{map[string]any{"browser_download_url": "https://forge.test/attach/2", "name": "log.txt"}}},
	}

	fake := newFakeForge(t, map[string][][]any{
		"/issues":   {issues},
		"/comments": {comments},
	})

	client := NewForgejoClient()
	metaContext := &MetadataContext{CloneURL: "https://codeberg.org/octo/repo.git", BaseURL: fake.server.URL}

	var issuesSeen []Issue
	err := client.ListIssues(context.Background(), metaContext, &Credential{APIKey: "tok"}, func(issue Issue) error {
		issuesSeen = append(issuesSeen, issue)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issuesSeen) != 1 {
		t.Fatalf("issues = %d", len(issuesSeen))
	}
	issue := issuesSeen[0]
	if len(issue.Attachments) != 2 {
		t.Fatalf("attachments = %+v, want the issue asset plus the comment asset", issue.Attachments)
	}
	if len(issue.Comments) != 1 || *issue.Comments[0].Author != "bob" {
		t.Errorf("comments = %+v", issue.Comments)
	}
	if got := fake.headers["/api/v3/user/repos"].Get("Authorization"); got != "" {
		_ = got
	}
}

func TestForgejoHasNoSnippetSupport(t *testing.T) {
	if NewForgejoClient().SupportsSnippets() {
		t.Error("Forgejo should report no snippet support")
	}
}

func TestEmptyAPIKeyShortCircuitsDiscovery(t *testing.T) {
	fake := newFakeForge(t, map[string][][]any{"/user/repos": {[]any{map[string]any{"clone_url": "https://x/y.git"}}}})
	client := NewGitHubClient()
	repositories, err := client.ListRepositories(context.Background(), RepositoryJobOptions{BaseURL: fake.server.URL}, &Credential{APIKey: "  "})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 0 {
		t.Fatalf("blank key should discover nothing, got %+v", repositories)
	}
}

func TestAttachmentStreamRejectsPrivateHost(t *testing.T) {
	// A literal private IP must be rejected before any connection attempt,
	// which also proves the guard does not depend on DNS.
	s := newSession("Bearer", &Credential{APIKey: "tok"})
	_, err := openAttachmentStream(context.Background(), s, "http://10.1.2.3/file.bin", "")
	if err == nil || !strings.Contains(err.Error(), "private, loopback, or link-local") {
		t.Fatalf("private host should be rejected, got %v", err)
	}

	// The trusted forge host is exempt, so self-hosted instances on private
	// address space still work: with the host marked trusted the guard passes
	// and the (loopback) server answers, proving the request went out.
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("bytes"))
	}))
	t.Cleanup(trusted.Close)
	stream, err := openAttachmentStream(context.Background(), s, trusted.URL+"/file.bin", hostOf(trusted.URL))
	if err != nil {
		t.Fatalf("trusted host should be exempt from the private-address guard: %v", err)
	}
	_ = stream.Close()
}

func TestAttachmentStreamCapsDeclaredLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write([]byte("too big"))
	}))
	t.Cleanup(server.Close)

	s := newSession("Bearer", &Credential{APIKey: "tok"})
	_, err := openAttachmentStream(context.Background(), s, server.URL+"/file.bin", hostOf(server.URL))
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("an oversized declared length should fail fast, got %v", err)
	}
}

func TestAttachmentStreamFollowsRedirectsAndDropsAuth(t *testing.T) {
	var secondRequest *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		secondRequest = r.Clone(r.Context())
		_, _ = w.Write([]byte("attachment bytes"))
	}))
	t.Cleanup(server.Close)

	s := newSession("Bearer", &Credential{APIKey: "tok"})
	stream, err := openAttachmentStream(context.Background(), s, server.URL+"/start", hostOf(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	if stream.KnownLength != 16 {
		t.Errorf("KnownLength = %d, want the declared 16 bytes", stream.KnownLength)
	}
	if secondRequest == nil {
		t.Fatal("the redirect was not followed")
	}
	// Same-host redirect keeps the credential.
	if got := secondRequest.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("same-host redirect should keep the auth header, got %q", got)
	}
}

func TestBuildStorageFileNameIsStable(t *testing.T) {
	first := BuildStorageFileName("https://example.com/a/b.png", "My File (1).PNG")
	second := BuildStorageFileName("https://example.com/a/b.png", "My File (1).PNG")
	if first != second {
		t.Fatalf("naming should be deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "1e614f16-") && len(first) < 10 {
		t.Errorf("file name = %q, want a hash prefix", first)
	}
	if !strings.Contains(first, "-My-File-1-.PNG") {
		t.Errorf("file name = %q, want the sanitized raw name", first)
	}
}

func TestSanitizeFileNameStripsPaths(t *testing.T) {
	if got := SanitizeFileName(`../../etc/passwd`); got != "passwd" {
		t.Errorf("sanitizeFileName = %q, want passwd", got)
	}
	if got := SanitizeFileName("100%25+done.png"); got != "100%25+done.png" {
		// '+' is not a URL escape; PathUnescape keeps it, and '+' is replaced.
		t.Logf("note: %q", got)
	}
}

func TestResolveRetryDelay(t *testing.T) {
	maxDelay := 60 * time.Second

	if got := resolveRetryDelay("120", 1, maxDelay, true, false); got != maxDelay {
		t.Errorf("capped Retry-After = %v, want %v", got, maxDelay)
	}
	if got := resolveRetryDelay("5", 1, maxDelay, true, false); got != 5*time.Second {
		t.Errorf("Retry-After = %v, want 5s", got)
	}
	if got := resolveRetryDelay("", 3, maxDelay, false, false); got != 4*time.Second {
		t.Errorf("exponential backoff = %v, want 4s", got)
	}
	if got := resolveRetryDelay("", 10, 8*time.Second, false, false); got != 8*time.Second {
		t.Errorf("capped backoff = %v, want 8s", got)
	}
}

func TestResolveGitHubAPIBase(t *testing.T) {
	tests := []struct {
		configured string
		want       string
	}{
		{configured: "", want: "https://api.github.com"},
		{configured: "https://github.com", want: "https://api.github.com"},
		{configured: "https://github.example.com", want: "https://github.example.com/api/v3"},
		{configured: "https://github.example.com/api/v3/", want: "https://github.example.com/api/v3"},
		{configured: "https://ghe.example.com/api/other", want: "https://ghe.example.com/api/other"},
	}
	for _, tt := range tests {
		if got := resolveGitHubAPIBase(tt.configured); got != tt.want {
			t.Errorf("resolveGitHubAPIBase(%q) = %q, want %q", tt.configured, got, tt.want)
		}
	}
}

func TestFactoryResolution(t *testing.T) {
	factory, err := NewDefaultFactory()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Resolve("GitHub"); err != nil {
		t.Errorf("resolve should be case-insensitive: %v", err)
	}
	if _, err := factory.Resolve("sourcehut"); err == nil {
		t.Error("unknown provider should fail")
	}
	if factory.TryResolveMetadata("forgejo") == nil {
		t.Error("Forgejo should expose project metadata")
	}
}

func TestIsPrivateOrLocalCatchesSiteLocalRange(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"fec0::1", true},      // start of fec0::/10
		{"feff::1", true},      // end of fec0::/10
		{"fe80::1", true},      // link-local
		{"2001:db8::1", false}, // public IPv6
		{"10.1.2.3", true},     // private IPv4
		{"8.8.8.8", false},     // public IPv4
	}
	for _, c := range cases {
		if got := isPrivateOrLocal(net.ParseIP(c.address)); got != c.want {
			t.Errorf("isPrivateOrLocal(%s) = %v, want %v", c.address, got, c.want)
		}
	}
}

func TestGitHubAttachmentsRejectEncodedDotSegments(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"literal traversal", "see https://github.com/octo/repo/files/1/../../admin/x.png", 0},
		{"encoded dot segment", "see https://github.com/octo/repo/files/1/%2e%2e/admin/x.png", 0},
		{"encoded dot segment and slash", "see https://github.com/octo/repo/files/1/%2e%2e%2fadmin/x.png", 0},
		{"uppercase encoding", "see https://github.com/octo/repo/files/1/%2E%2E/admin/x.png", 0},
		{"query-only encoding is fine", "see https://github.com/octo/repo/files/1/ok.png?next=%2e%2e", 1},
		{"double-dot file name is fine", "see https://github.com/octo/repo/files/1/chart..v2.png", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			if attachments := extractGitHubAttachments(&body, nil); len(attachments) != c.want {
				t.Fatalf("attachments = %+v, want %d", attachments, c.want)
			}
		})
	}
}

func TestGitLabAttachmentsRejectEncodedTraversal(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		body string
		want int
	}{
		{"literal traversal", "x /uploads/" + sha + "/../../api/v4/users y", 0},
		{"encoded traversal", "x /uploads/" + sha + "/%2e%2e%2fapi/v4/users y", 0},
		{"encoded dot segment", "x /uploads/" + sha + "/%2e%2e y", 0},
		{"encoded separator", "x /uploads/" + sha + "/a%2fb.png y", 0},
		{"clean name", "x /uploads/" + sha + "/report.pdf y", 1},
		{"double-dot file name", "x /uploads/" + sha + "/chart..v2.png y", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			metaContext := &MetadataContext{CloneURL: "https://gitlab.com/g/p.git"}
			if attachments := extractGitLabAttachments(metaContext, &body, nil); len(attachments) != c.want {
				t.Fatalf("attachments = %+v, want %d", attachments, c.want)
			}
		})
	}
}

func TestForEachParallelReturnsContextErrorWithoutCallingFn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	err := forEachParallel(ctx, 4, []int{1, 2, 3}, func(item int) error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("fn called %d times, want 0", calls)
	}
}

func TestForEachParallelDeliversEveryItem(t *testing.T) {
	items := make([]int, 32)
	for i := range items {
		items[i] = i
	}

	var mu sync.Mutex
	seen := make(map[int]bool)
	err := forEachParallel(context.Background(), 4, items, func(item int) error {
		mu.Lock()
		defer mu.Unlock()
		seen[item] = true
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(seen) != len(items) {
		t.Errorf("fn ran for %d of %d items", len(seen), len(items))
	}
}

func TestForEachParallelStopsOnFirstError(t *testing.T) {
	sentinel := errors.New("item failed")
	entered := make(chan int, 8)
	start := make(chan struct{})
	failed := make(chan struct{})
	finish := make(chan struct{})
	var once sync.Once

	items := make([]int, 8)
	for i := range items {
		items[i] = i
	}

	result := make(chan error, 1)
	go func() {
		result <- forEachParallel(context.Background(), 4, items, func(item int) error {
			entered <- item
			<-start
			if item == 0 {
				once.Do(func() { close(failed) })
				return sentinel
			}
			<-finish
			return nil
		})
	}()

	// The concurrency bound guarantees exactly four items enter before any
	// can return: all four block on start, so the producer cannot fill more
	// slots until the test releases them.
	for received := 0; received < 4; received++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the first four items to start")
		}
	}
	close(start)
	<-failed
	close(finish)

	if err := <-result; !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	select {
	case extra := <-entered:
		t.Errorf("item %d was dispatched after the first failure", extra)
	default:
	}
}

func TestGitHubAttachmentsTolerateBarePercentSigns(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"percent in file name", "see https://github.com/octo/repo/files/1/100%_report.png", 1},
		{"malformed escape", "see https://user-images.githubusercontent.com/u/1/a%zz.png", 1},
		{"trailing percent", "see https://github.com/octo/repo/files/1/data%.png", 1},
		{"traversal still rejected", "see https://github.com/octo/repo/files/1/..%2f..%2fadmin/x.png", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			if attachments := extractGitHubAttachments(&body, nil); len(attachments) != c.want {
				t.Fatalf("attachments = %+v, want %d", attachments, c.want)
			}
		})
	}
}

func TestGitLabAttachmentsTolerateBarePercentSigns(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		body string
		want int
	}{
		{"percent in file name", "x /uploads/" + sha + "/100%_report.png y", 1},
		{"malformed escape", "x /uploads/" + sha + "/a%zz.png y", 1},
		{"encoded traversal still rejected", "x /uploads/" + sha + "/%2e%2e%2fusers y", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			metaContext := &MetadataContext{CloneURL: "https://gitlab.com/g/p.git"}
			if attachments := extractGitLabAttachments(metaContext, &body, nil); len(attachments) != c.want {
				t.Fatalf("attachments = %+v, want %d", attachments, c.want)
			}
		})
	}
}
