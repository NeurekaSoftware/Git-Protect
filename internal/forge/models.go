// Package forge discovers repositories on GitHub, GitLab, and Forgejo and
// fetches their metadata (issues, merge/pull requests, releases, and
// attachments) over each forge's REST API.
//
// All providers share one HTTP layer (pooled connections, retry with
// Retry-After awareness, pagination helpers) and differ only in endpoint
// shapes, field names, and the authentication header scheme.
package forge

import (
	"context"
	"sync"
	"time"
)

// DiscoveredRepositoryKind distinguishes the resource kinds a provider can
// discover.
type DiscoveredRepositoryKind int

const (
	KindRepository DiscoveredRepositoryKind = iota
	KindGist
	KindSnippet
)

// DiscoveredRepository is one resource found during a provider's discovery
// walk.
type DiscoveredRepository struct {
	CloneURL string
	WebURL   string
	Kind     DiscoveredRepositoryKind
	// Identifier is the gist/snippet id used to build the storage key for
	// those resources.
	Identifier string
	// ParentURL is the owning project's web URL for a project snippet, used
	// to nest it under that project's storage prefix. Empty for gists and
	// personal snippets.
	ParentURL string
	// ProviderProjectID is a provider-native project identifier (GitLab's
	// numeric id) used to fetch project metadata. Empty when the provider
	// addresses projects by owner/repo path instead.
	ProviderProjectID string
	// IsStarred is true when this repository was discovered only because it
	// is starred, not owned. Project metadata is never backed up for starred
	// repositories.
	IsStarred bool
}

// MetadataContext carries everything a metadata client needs to address one
// project's issues and merge requests. The sync orchestrator builds one per
// repository from the discovered repository plus the job's configured base
// URL.
type MetadataContext struct {
	CloneURL          string
	WebURL            string
	ProviderProjectID string
	// BaseURL is the self-hosted forge base URL from the job config, empty
	// for the provider default.
	BaseURL string
	// Concurrency bounds how many issues/merge requests are fetched
	// (comments) and populated in parallel for this project. 1 = sequential.
	Concurrency int
	// DownloadThrottle is shared across the whole run to cap how many
	// attachments are downloaded at once, so nested repository x metadata
	// parallelism cannot multiply peak memory. Nil disables throttling.
	DownloadThrottle Throttle

	// commentCache holds the GitHub repo-wide comment walk shared by the
	// issue and pull-request passes; it lives as long as the context, i.e.
	// the repository's metadata sync.
	mu           sync.Mutex
	commentCache *commentCache
}

// commentCacheFor lazily creates the per-repository GitHub comment cache.
func (c *MetadataContext) commentCacheFor() *commentCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commentCache == nil {
		c.commentCache = &commentCache{}
	}
	return c.commentCache
}

// Throttle is a counting semaphore for bounded downloads. A zero-value or nil
// throttle disables limiting.
type Throttle chan struct{}

// NewThrottle returns a Throttle admitting n concurrent holders.
func NewThrottle(n int) Throttle {
	if n < 1 {
		n = 1
	}
	return make(chan struct{}, n)
}

// Acquire occupies one slot, blocking until available or ctx is done.
func (t Throttle) Acquire(ctx context.Context) error {
	if t == nil {
		return nil
	}
	select {
	case t <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees one slot.
func (t Throttle) Release() {
	if t == nil {
		return
	}
	select {
	case <-t:
	default:
	}
}

// Comment is a single comment (GitLab note, GitHub/Forgejo comment) on an
// issue or merge request. The JSON shape matches the documented backup format
// field-for-field: nullable values serialize as null and list-valued fields as
// empty arrays when absent.
type Comment struct {
	ID        *int64     `json:"id"`
	Author    *string    `json:"author"`
	Body      *string    `json:"body"`
	CreatedAt *time.Time `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt"`
	// System is true for provider-generated notes (state changes, label
	// edits) rather than human comments.
	System bool `json:"system"`
}

// Attachment is a file attached to an issue/MR body or comment. The provider
// resolves DownloadURL (not serialized); the orchestrator downloads it and
// records StorageKey — the stored object's key relative to the bucket — so a
// consumer can link the original reference to the backed-up file.
type Attachment struct {
	FileName string `json:"fileName"`
	// OriginalPath is the reference as it appears in the source text, e.g.
	// /uploads/{sha}/{filename}.
	OriginalPath string  `json:"originalPath"`
	StorageKey   *string `json:"storageKey"`
	SizeBytes    *int64  `json:"sizeBytes"`
	ContentType  *string `json:"contentType"`
	// DownloadURL is not serialized: it is a fetch instruction, not data.
	DownloadURL string `json:"-"`
	// Downloadable is false when the reference is recorded but the file is
	// never downloaded (e.g. a GitLab release asset link pointing outside the
	// instance, so the credential is never sent to a third party).
	Downloadable bool `json:"-"`
}

// Issue is a backed-up issue with its full comment thread embedded. Serialized
// as {number}.json. Fields are normalized across providers so a consumer can
// render them uniformly.
type Issue struct {
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	State     *string    `json:"state"`
	Author    *string    `json:"author"`
	Body      *string    `json:"body"`
	CreatedAt *time.Time `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt"`
	ClosedAt  *time.Time `json:"closedAt"`
	Labels    []string   `json:"labels"`
	WebURL    *string    `json:"webUrl"`

	// Comments and Attachments are populated by the provider/orchestrator
	// after the per-issue fetches complete.
	Comments    []Comment    `json:"comments"`
	Attachments []Attachment `json:"attachments"`
}

// MergeRequest is a backed-up merge/pull request with its comment thread
// embedded. Serialized as {number}.json under merge-requests/.
type MergeRequest struct {
	Number       int64      `json:"number"`
	Title        string     `json:"title"`
	State        *string    `json:"state"`
	Author       *string    `json:"author"`
	Body         *string    `json:"body"`
	SourceBranch *string    `json:"sourceBranch"`
	TargetBranch *string    `json:"targetBranch"`
	CreatedAt    *time.Time `json:"createdAt"`
	UpdatedAt    *time.Time `json:"updatedAt"`
	MergedAt     *time.Time `json:"mergedAt"`
	ClosedAt     *time.Time `json:"closedAt"`
	Labels       []string   `json:"labels"`
	WebURL       *string    `json:"webUrl"`

	Comments    []Comment    `json:"comments"`
	Attachments []Attachment `json:"attachments"`
}

// Release is a backed-up release with its asset references. Serialized as
// {sanitized-tag}.json under releases/.
type Release struct {
	Tag         string       `json:"tag"`
	Name        *string      `json:"name"`
	Body        *string      `json:"body"`
	Author      *string      `json:"author"`
	Draft       *bool        `json:"draft"`
	Prerelease  *bool        `json:"prerelease"`
	CreatedAt   *time.Time   `json:"createdAt"`
	PublishedAt *time.Time   `json:"publishedAt"`
	Commit      *string      `json:"commit"`
	WebURL      *string      `json:"webUrl"`
	Attachments []Attachment `json:"attachments"`
}

// RepositoryProviderClient discovers repositories on one forge.
type RepositoryProviderClient interface {
	Provider() string

	// SupportsSnippets reports whether the provider exposes gists or
	// snippets. False means an includeSnippets job is reported as unsupported
	// rather than silently discovering nothing.
	SupportsSnippets() bool

	ListRepositories(ctx context.Context, repository RepositoryJobOptions, credential *Credential) ([]DiscoveredRepository, error)
}

// RepositoryJobOptions is the slice of a repository job's configuration the
// discovery walk needs.
type RepositoryJobOptions struct {
	BaseURL         string
	IncludeStarred  bool
	IncludeSnippets bool
}

// Credential is the resolved forge credential used for API authentication.
type Credential struct {
	Username string
	APIKey   string
}

// ProjectMetadataProviderClient fetches a project's issues, merge/pull
// requests, and releases from a forge API. Items are delivered through
// callbacks so the caller can back up and release each item without the whole
// collection being held in memory at once.
type ProjectMetadataProviderClient interface {
	Provider() string

	SupportsIssues() bool
	SupportsMergeRequests() bool
	SupportsReleases() bool
	// SupportsArtifacts reports whether the provider can resolve and download
	// attachments/assets referenced by the metadata.
	SupportsArtifacts() bool

	ListIssues(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Issue) error) error

	ListMergeRequests(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(MergeRequest) error) error

	ListReleases(ctx context.Context, context *MetadataContext, credential *Credential, deliver func(Release) error) error

	// OpenAttachment opens the raw bytes of a previously reported attachment
	// for streaming upload. The caller must Close the result. The provider
	// owns authentication and any host/redirect quirks; the caller owns
	// storage.
	OpenAttachment(ctx context.Context, context *MetadataContext, credential *Credential, downloadURL string) (*AttachmentStream, error)
}
