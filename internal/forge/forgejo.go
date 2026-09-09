package forge

import (
	"context"
	"fmt"
	"log/slog"
)

// ForgejoClient discovers repositories and fetches metadata from Forgejo /
// Gitea instances (codeberg.org by default, /api/v1).
type ForgejoClient struct{}

// NewForgejoClient returns the Forgejo provider client.
func NewForgejoClient() *ForgejoClient { return &ForgejoClient{} }

const (
	forgejoDefaultBaseURL = "https://codeberg.org"
	forgejoPageSize       = 50
	forgejoAuthScheme     = "token"
)

func (c *ForgejoClient) Provider() string { return "forgejo" }

// Forgejo has no gists/snippets API, so includeSnippets cannot be honored
// here.
func (c *ForgejoClient) SupportsSnippets() bool      { return false }
func (c *ForgejoClient) SupportsIssues() bool        { return true }
func (c *ForgejoClient) SupportsMergeRequests() bool { return true }
func (c *ForgejoClient) SupportsReleases() bool      { return true }
func (c *ForgejoClient) SupportsArtifacts() bool     { return true }

// ListRepositories walks the owned (and optionally starred) endpoints
// concurrently and merges the results owned-first.
func (c *ForgejoClient) ListRepositories(ctx context.Context, repository RepositoryJobOptions, credential *Credential) ([]DiscoveredRepository, error) {
	if !hasAPIKey(credential) {
		return nil, nil
	}

	baseURL := resolveForgejoAPIBase(repository.BaseURL)
	s := newSession(forgejoAuthScheme, credential)

	walks := []func() ([]DiscoveredRepository, error){
		func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/user/repos?affiliation=owner&limit=%d&page=%d", baseURL, forgejoPageSize, page)
			}, pageIsFull(forgejoPageSize), func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGiteaRepository(item, false)
			})
		},
	}

	if repository.IncludeStarred {
		slog.Debug("Including starred repositories.", "provider", c.Provider())
		walks = append(walks, func() ([]DiscoveredRepository, error) {
			return s.collectDiscovered(ctx, func(page int) string {
				return fmt.Sprintf("%s/user/starred?limit=%d&page=%d", baseURL, forgejoPageSize, page)
			}, pageIsFull(forgejoPageSize), func(item map[string]any) (DiscoveredRepository, bool) {
				return mapGiteaRepository(item, true)
			})
		})
	}

	return mergeDiscoveryWalks(walks...)
}

// ListIssues streams issues, populating each one's shared comment thread (with
// its asset attachments) in parallel up to the configured concurrency.
func (c *ForgejoClient) ListIssues(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Issue) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveForgejoAPIBase(context.BaseURL)
	s := newSession(forgejoAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	return collectItems(ctx, s, context.Concurrency,
		func(page int) string {
			return fmt.Sprintf("%s/repos/%s/issues?type=issues&state=all&limit=%d&page=%d", baseURL, repositoryPath, forgejoPageSize, page)
		},
		pageIsFull(forgejoPageSize),
		mapForgejoIssue,
		func(issue *Issue) error {
			comments, commentAttachments, err := fetchForgejoComments(ctx, s, baseURL, repositoryPath, issue.Number)
			if err != nil {
				return err
			}
			issue.Comments = comments
			issue.Attachments = mergeAttachments(issue.Attachments, commentAttachments)
			return nil
		},
		func(issue *Issue) error { return deliver(*issue) })
}

// ListMergeRequests streams pull requests, sharing the issue comment thread in
// the Gitea/Forgejo API.
func (c *ForgejoClient) ListMergeRequests(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(MergeRequest) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveForgejoAPIBase(context.BaseURL)
	s := newSession(forgejoAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	return collectItems(ctx, s, context.Concurrency,
		func(page int) string {
			return fmt.Sprintf("%s/repos/%s/pulls?state=all&limit=%d&page=%d", baseURL, repositoryPath, forgejoPageSize, page)
		},
		pageIsFull(forgejoPageSize),
		mapForgejoPullRequest,
		func(pull *MergeRequest) error {
			comments, commentAttachments, err := fetchForgejoComments(ctx, s, baseURL, repositoryPath, pull.Number)
			if err != nil {
				return err
			}
			pull.Comments = comments
			pull.Attachments = mergeAttachments(pull.Attachments, commentAttachments)
			return nil
		},
		func(pull *MergeRequest) error { return deliver(*pull) })
}

func (c *ForgejoClient) ListReleases(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Release) error) error {
	if !hasAPIKey(credential) {
		return nil
	}

	baseURL := resolveForgejoAPIBase(context.BaseURL)
	s := newSession(forgejoAuthScheme, credential)
	repositoryPath, err := buildOwnerRepoPath(context.CloneURL)
	if err != nil {
		return err
	}

	return s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/releases?limit=%d&page=%d", baseURL, repositoryPath, forgejoPageSize, page)
	}, pageIsFull(forgejoPageSize), func(item map[string]any) error {
		release, ok := mapGiteaRelease(item)
		if !ok {
			return nil
		}
		return deliver(*release)
	})
}

// OpenAttachment opens a Forgejo-hosted attachment stream.
func (c *ForgejoClient) OpenAttachment(ctx context.Context, context *MetadataContext, credential *Credential, downloadURL string) (*AttachmentStream, error) {
	s := newSession(forgejoAuthScheme, credential)
	return openAttachmentStream(ctx, s, downloadURL, resolveInstanceHost(context))
}

// fetchForgejoComments collects an issue/PR's comments and the asset
// attachments carried by each of them.
func fetchForgejoComments(ctx context.Context, s *session, baseURL, repositoryPath string, number int64) ([]Comment, []Attachment, error) {
	var comments []Comment
	var attachments []Attachment

	err := s.stream(ctx, func(page int) string {
		return fmt.Sprintf("%s/repos/%s/issues/%d/comments?limit=%d&page=%d", baseURL, repositoryPath, number, forgejoPageSize, page)
	}, pageIsFull(forgejoPageSize), func(item map[string]any) error {
		attachments = append(attachments, extractAssetArray(item)...)
		if comment, ok := mapGiteaComment(item); ok {
			comments = append(comments, comment)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return comments, attachments, nil
}

func mapForgejoIssue(item map[string]any) (*Issue, bool) {
	issue, ok := mapGiteaIssue(item)
	if ok {
		issue.Attachments = extractAssetArray(item)
	}
	return issue, ok
}

func mapForgejoPullRequest(item map[string]any) (*MergeRequest, bool) {
	pull, ok := mapGiteaPullRequest(item)
	if ok {
		pull.Attachments = extractAssetArray(item)
	}
	return pull, ok
}

// resolveForgejoAPIBase composes the Forgejo API base: default codeberg.org +
// /api/v1, or the configured instance + /api/v1.
func resolveForgejoAPIBase(configuredBaseURL string) string {
	return composeAPIBaseURL(configuredBaseURL, forgejoDefaultBaseURL, "/api/v1")
}
