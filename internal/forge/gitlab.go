package forge

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/neurekadev/git-backup/internal/paths"
)

// GitLabClient discovers repositories and fetches metadata from GitLab's REST
// API (gitlab.com + /api/v4, or the configured instance + /api/v4).
type GitLabClient struct{}

// NewGitLabClient returns the GitLab provider client.
func NewGitLabClient() *GitLabClient { return &GitLabClient{} }

const (
	gitlabDefaultBaseURL = "https://gitlab.com"
	gitlabPageSize       = 100
	gitlabAuthScheme     = "Bearer"
)

// gitlabUploadReference matches GitLab's upload references,
// /uploads/{32-hex-sha}/{filename}, inside issue, merge request, and note
// bodies. The filename runs until whitespace or a markdown/HTML delimiter.
var gitlabUploadReference = regexp.MustCompile(`/uploads/([0-9a-fA-F]{32})/([^\s)\]"'<>]+)`)

func (c *GitLabClient) Provider() string            { return "gitlab" }
func (c *GitLabClient) SupportsSnippets() bool      { return true }
func (c *GitLabClient) SupportsIssues() bool        { return true }
func (c *GitLabClient) SupportsMergeRequests() bool { return true }
func (c *GitLabClient) SupportsReleases() bool      { return true }
func (c *GitLabClient) SupportsArtifacts() bool     { return true }

// ListRepositories walks the owned (and optionally starred, snippet) endpoints
// concurrently and merges the results owned-first.
func (c *GitLabClient) ListRepositories(ctx context.Context, repository RepositoryJobOptions, credential *Credential) ([]DiscoveredRepository, error) {
	if !hasAPIKey(credential) {
		return nil, nil
	}

	baseURL := resolveGitLabAPIBase(repository.BaseURL)
	s := newSession(gitlabAuthScheme, credential)

	walks := []func() ([]DiscoveredRepository, error){
		func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/projects?owned=true&simple=true&per_page=%d&page=%d", baseURL, gitlabPageSize, page)
			}, gitLabHasNextPage, func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGitLabProject(item, false)
			})
		},
	}

	if repository.IncludeStarred {
		slog.Debug("Including starred repositories.", "provider", c.Provider())
		walks = append(walks, func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/projects?starred=true&simple=true&per_page=%d&page=%d", baseURL, gitlabPageSize, page)
			}, gitLabHasNextPage, func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGitLabProject(item, true)
			})
		})
	}

	if repository.IncludeSnippets {
		// GitLab has no "starred snippets" endpoint, so includeStarred adds
		// nothing here.
		slog.Debug("Including snippets.", "provider", c.Provider())
		walks = append(walks, func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/snippets?per_page=%d&page=%d", baseURL, gitlabPageSize, page)
			}, gitLabHasNextPage, mapGitLabSnippet)
		})
	}

	return mergeDiscoveryWalks(walks...)
}

// ListIssues streams issues, populating each one's notes and upload
// attachments in parallel up to the configured concurrency.
func (c *GitLabClient) ListIssues(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Issue) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitLabAPIBase(context.BaseURL)
	s := newSession(gitlabAuthScheme, credential)
	projectID, err := resolveGitLabProjectIdentifier(context)
	if err != nil {
		return err
	}

	return collectItems(ctx, s, context.Concurrency,
		func(page int) string {
			return fmt.Sprintf("%s/projects/%s/issues?per_page=%d&page=%d", baseURL, projectID, gitlabPageSize, page)
		},
		gitLabHasNextPage,
		mapGitLabIssue,
		func(issue *Issue) error {
			comments, err := s.collect(ctx, func(page int) string {
				return fmt.Sprintf("%s/projects/%s/issues/%d/notes?per_page=%d&sort=asc&page=%d", baseURL, projectID, issue.Number, gitlabPageSize, page)
			}, gitLabHasNextPage)
			if err != nil {
				return err
			}
			issue.Comments = mapComments(comments, mapGitLabNote)
			issue.Attachments = extractGitLabAttachments(context, issue.Body, issue.Comments)
			return nil
		},
		func(issue *Issue) error { return deliver(*issue) })
}

// ListMergeRequests streams merge requests, populating each one's notes and
// upload attachments in parallel up to the configured concurrency.
func (c *GitLabClient) ListMergeRequests(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(MergeRequest) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitLabAPIBase(context.BaseURL)
	s := newSession(gitlabAuthScheme, credential)
	projectID, err := resolveGitLabProjectIdentifier(context)
	if err != nil {
		return err
	}

	return collectItems(ctx, s, context.Concurrency,
		func(page int) string {
			return fmt.Sprintf("%s/projects/%s/merge_requests?per_page=%d&page=%d", baseURL, projectID, gitlabPageSize, page)
		},
		gitLabHasNextPage,
		mapGitLabMergeRequest,
		func(mergeRequest *MergeRequest) error {
			comments, err := s.collect(ctx, func(page int) string {
				return fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes?per_page=%d&sort=asc&page=%d", baseURL, projectID, mergeRequest.Number, gitlabPageSize, page)
			}, gitLabHasNextPage)
			if err != nil {
				return err
			}
			mergeRequest.Comments = mapComments(comments, mapGitLabNote)
			mergeRequest.Attachments = extractGitLabAttachments(context, mergeRequest.Body, mergeRequest.Comments)
			return nil
		},
		func(mergeRequest *MergeRequest) error { return deliver(*mergeRequest) })
}

// ListReleases streams releases with their asset links; instance-host links
// are downloadable, external links are recorded as references only.
func (c *GitLabClient) ListReleases(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Release) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveGitLabAPIBase(context.BaseURL)
	s := newSession(gitlabAuthScheme, credential)
	projectID, err := resolveGitLabProjectIdentifier(context)
	if err != nil {
		return err
	}
	instanceHost := resolveInstanceHost(context)

	return s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/projects/%s/releases?per_page=%d&page=%d", baseURL, projectID, gitlabPageSize, page)
	}, gitLabHasNextPage, func(item map[string]any) error {
		release, ok := mapGitLabRelease(item, instanceHost)
		if !ok {
			return nil
		}
		return deliver(*release)
	})
}

// OpenAttachment opens a GitLab-hosted attachment stream.
func (c *GitLabClient) OpenAttachment(ctx context.Context, context *MetadataContext, credential *Credential, downloadURL string) (*AttachmentStream, error) {
	s := newSession(gitlabAuthScheme, credential)
	return openAttachmentStream(ctx, s, downloadURL, resolveInstanceHost(context))
}

// mapComments maps raw comment items through mapper, skipping unmappable
// entries.
func mapComments(items []map[string]any, mapper func(map[string]any) (Comment, bool)) []Comment {
	var comments []Comment
	for _, item := range items {
		if comment, ok := mapper(item); ok {
			comments = append(comments, comment)
		}
	}
	return comments
}

func mapGitLabProject(item map[string]any, isStarred bool) (DiscoveredRepository, bool) {
	cloneURL, _ := jsonString(item, "http_url_to_repo")
	if cloneURL == "" {
		return DiscoveredRepository{}, false
	}

	webURL, _ := jsonString(item, "web_url")
	projectID := ""
	if id, ok := jsonInt64(item, "id"); ok {
		projectID = fmt.Sprintf("%d", id)
	}
	return DiscoveredRepository{
		CloneURL:          cloneURL,
		WebURL:            webURL,
		ProviderProjectID: projectID,
		IsStarred:         isStarred,
	}, true
}

func mapGitLabSnippet(item map[string]any) (DiscoveredRepository, bool) {
	// The list endpoint (GET /snippets) omits http_url_to_repo, so clone via
	// the web URL + ".git".
	id, _ := jsonRawID(item, "id")
	webURL, _ := jsonString(item, "web_url")
	if id == "" || webURL == "" {
		return DiscoveredRepository{}, false
	}

	// A non-null project_id marks a project snippet, which nests under its
	// owning project.
	_, isProjectSnippet := item["project_id"].(float64)

	return DiscoveredRepository{
		CloneURL:   webURL + ".git",
		WebURL:     webURL,
		Kind:       KindSnippet,
		Identifier: id,
		ParentURL:  trimSnippetSuffix(isProjectSnippet, webURL),
	}, true
}

// trimSnippetSuffix extracts the owning project's URL from a project snippet
// web URL, which looks like https://host/<namespace>/<project>/-/snippets/<id>
// (or legacy https://host/<namespace>/<project>/snippets/<id>).
func trimSnippetSuffix(isProjectSnippet bool, webURL string) string {
	if !isProjectSnippet {
		return ""
	}
	marker := strings.Index(webURL, "/-/snippets/")
	if marker < 0 {
		marker = strings.LastIndex(webURL, "/snippets/")
	}
	if marker > 0 {
		return webURL[:marker]
	}
	return ""
}

func mapGitLabIssue(item map[string]any) (*Issue, bool) {
	number, ok := jsonInt64(item, "iid")
	if !ok {
		number, ok = jsonInt64(item, "id")
	}
	title, _ := jsonString(item, "title")
	if !ok || title == "" {
		return nil, false
	}

	author, hasAuthor := jsonNestedString(item, "author", "username")
	createdAt, _ := jsonTime(item, "created_at")
	updatedAt, _ := jsonTime(item, "updated_at")
	closedAt, _ := jsonTime(item, "closed_at")
	webURL, _ := jsonString(item, "web_url")
	state, hasState := jsonString(item, "state")
	body, hasBody := jsonString(item, "description")
	return &Issue{
		Number:    number,
		Title:     title,
		State:     stringPtr(state, hasState),
		Author:    stringPtr(author, hasAuthor),
		Body:      stringPtr(body, hasBody),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		ClosedAt:  closedAt,
		Labels:    jsonLabels(item, "labels"),
		WebURL:    stringPtr(webURL, webURL != ""),
	}, true
}

func mapGitLabMergeRequest(item map[string]any) (*MergeRequest, bool) {
	number, ok := jsonInt64(item, "iid")
	if !ok {
		number, ok = jsonInt64(item, "id")
	}
	title, _ := jsonString(item, "title")
	if !ok || title == "" {
		return nil, false
	}

	author, hasAuthor := jsonNestedString(item, "author", "username")
	sourceBranch, hasSource := jsonString(item, "source_branch")
	targetBranch, hasTarget := jsonString(item, "target_branch")
	createdAt, _ := jsonTime(item, "created_at")
	updatedAt, _ := jsonTime(item, "updated_at")
	mergedAt, _ := jsonTime(item, "merged_at")
	closedAt, _ := jsonTime(item, "closed_at")
	webURL, _ := jsonString(item, "web_url")
	state, hasState := jsonString(item, "state")
	body, hasBody := jsonString(item, "description")
	return &MergeRequest{
		Number:       number,
		Title:        title,
		State:        stringPtr(state, hasState),
		Author:       stringPtr(author, hasAuthor),
		Body:         stringPtr(body, hasBody),
		SourceBranch: stringPtr(sourceBranch, hasSource),
		TargetBranch: stringPtr(targetBranch, hasTarget),
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		MergedAt:     mergedAt,
		ClosedAt:     closedAt,
		Labels:       jsonLabels(item, "labels"),
		WebURL:       stringPtr(webURL, webURL != ""),
	}, true
}

// mapGitLabNote maps a GitLab note: author at author.username, with the system
// flag distinguishing generated notes.
func mapGitLabNote(item map[string]any) (Comment, bool) {
	return mapComment(item, "author", "username", true)
}

func mapGitLabRelease(item map[string]any, instanceHost string) (*Release, bool) {
	tag, _ := jsonString(item, "tag_name")
	if tag == "" {
		return nil, false
	}

	name, hasName := jsonString(item, "name")
	author, hasAuthor := jsonNestedString(item, "author", "username")
	createdAt, _ := jsonTime(item, "created_at")
	publishedAt, _ := jsonTime(item, "released_at")
	commit, hasCommit := jsonNestedString(item, "commit", "id")
	webURL, hasWebURL := jsonNestedString(item, "_links", "self")
	body, hasBody := jsonString(item, "description")
	return &Release{
		Tag:         tag,
		Name:        stringPtr(name, hasName),
		Body:        stringPtr(body, hasBody),
		Author:      stringPtr(author, hasAuthor),
		CreatedAt:   createdAt,
		PublishedAt: publishedAt,
		Commit:      stringPtr(commit, hasCommit),
		WebURL:      stringPtr(webURL, hasWebURL),
		Attachments: extractGitLabReleaseLinks(item, instanceHost),
	}, true
}

// extractGitLabReleaseLinks extracts GitLab release asset links. Only links
// whose target is on the GitLab instance are marked downloadable — external
// links are recorded as references so the private token is never sent to a
// third-party host. Auto-generated source archives (assets.sources) are
// skipped, since the repository mirror already captures every tag's source.
func extractGitLabReleaseLinks(item map[string]any, instanceHost string) []Attachment {
	assets, ok := item["assets"].(map[string]any)
	if !ok {
		return nil
	}
	links, ok := assets["links"].([]any)
	if !ok {
		return nil
	}

	var attachments []Attachment
	seen := make(map[string]struct{}, len(links))
	for _, element := range links {
		link, ok := element.(map[string]any)
		if !ok {
			continue
		}
		name, _ := jsonString(link, "name")
		url, _ := jsonString(link, "url")
		directURL, _ := jsonString(link, "direct_asset_url")
		reference := url
		if reference == "" {
			reference = directURL
		}
		fetchURL := directURL
		if fetchURL == "" {
			fetchURL = url
		}
		if name == "" || reference == "" || fetchURL == "" {
			continue
		}
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}

		target := url
		if target == "" {
			target = directURL
		}
		attachments = append(attachments, Attachment{
			FileName:     BuildStorageFileName(reference, name),
			OriginalPath: reference,
			DownloadURL:  fetchURL,
			Downloadable: isInstanceHost(target, instanceHost),
		})
	}
	return attachments
}

func isInstanceHost(rawURL, instanceHost string) bool {
	if instanceHost == "" {
		return false
	}
	host := hostOf(rawURL)
	return host != "" && strings.EqualFold(host, instanceHost)
}

// extractGitLabAttachments scans an issue/MR body and its notes for upload
// references, building project-relative download URLs.
func extractGitLabAttachments(context *MetadataContext, body *string, comments []Comment) []Attachment {
	projectURL := context.WebURL
	if projectURL == "" {
		projectURL = paths.TrimGitSuffix(context.CloneURL)
	}
	projectURL = strings.TrimSuffix(projectURL, "/")

	return scanBodyAndComments(body, comments, gitlabUploadReference, func(match string) (Attachment, bool) {
		groups := gitlabUploadReference.FindStringSubmatch(match)
		if len(groups) != 3 {
			return Attachment{}, false
		}
		sha := groups[1]
		rawName := groups[2]

		// The name comes from untrusted issue/comment text and is decoded
		// before checking, so an encoded separator or dot-segment (e.g.
		// '%2e%2e') cannot smuggle traversal past the raw-text check. A '/'
		// or '..' would survive into the download URL, where URL dot-segment
		// normalization walks the token-bearing request off the uploads path
		// to any endpoint on the instance (e.g. /api/v4/users) — the host is
		// unchanged, so the request still counts as same-origin and keeps the
		// credential.
		decodedName, err := url.PathUnescape(rawName)
		if err != nil {
			return Attachment{}, false
		}
		if strings.Contains(decodedName, "/") || strings.Contains(decodedName, `\`) || decodedName == "." || decodedName == ".." {
			return Attachment{}, false
		}

		originalPath := "/uploads/" + sha + "/" + rawName
		return Attachment{
			FileName:     sha[:8] + "-" + SanitizeFileName(rawName),
			OriginalPath: originalPath,
			DownloadURL:  projectURL + originalPath,
		}, true
	})
}

// resolveGitLabAPIBase composes the GitLab API base: default gitlab.com +
// /api/v4, or the configured instance + /api/v4.
func resolveGitLabAPIBase(configuredBaseURL string) string {
	return composeAPIBaseURL(configuredBaseURL, gitlabDefaultBaseURL, "/api/v4")
}

// resolveGitLabProjectIdentifier resolves the project addressing token: the
// numeric id when discovery captured one, otherwise the URL-encoded project
// path (namespace/project), which GitLab accepts in place of the numeric id.
// The fallback keeps issue backup working even if discovery did not capture
// the id for some reason.
func resolveGitLabProjectIdentifier(context *MetadataContext) (string, error) {
	if context.ProviderProjectID != "" {
		return strings.TrimSpace(context.ProviderProjectID), nil
	}

	parsed, err := parseAbsoluteURL(context.CloneURL)
	if err != nil {
		return "", fmt.Errorf("cannot resolve a GitLab project id from '%s'.", context.CloneURL)
	}

	path := paths.TrimGitSuffix(strings.Trim(parsed.Path, "/"))
	return queryEscapePath(path), nil
}
