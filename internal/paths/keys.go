package paths

import (
	"strconv"
	"strings"
)

// The two storage roots. Retention scans by these prefixes and the builders
// below write them, so both are derived from one constant per root: a literal
// here and a constant there would agree only by spelling, and a change to one
// would leave retention scanning a prefix nothing is written under.
const (
	RepositoriesRoot   = "repositories"
	SnippetsRoot       = "snippets"
	RepositoriesPrefix = RepositoriesRoot + "/"
	SnippetsPrefix     = SnippetsRoot + "/"

	IssuesCollectionSegment        = "issues"
	MergeRequestsCollectionSegment = "merge-requests"
	ReleasesCollectionSegment      = "releases"
	AttachmentsCollectionSegment   = "attachments"
	CollectionManifestObjectName   = "index.json"

	// ArchiveObjectNameSuffix marks snapshot archive keys; retention decodes
	// the snapshot timestamp out of the name.
	ArchiveObjectNameSuffix = "_repo.tar.gz"
	// RepositoryMetadataObjectName is the advisory metadata document stored
	// directly under each repository prefix.
	RepositoryMetadataObjectName = "metadata.json"
)

// BuildProviderRepositoryPrefix returns
// repositories/provider/{provider}/{hierarchy}.
func BuildProviderRepositoryPrefix(provider string, repository PathInfo) string {
	segments := append([]string{RepositoriesRoot, "provider", strings.ToLower(strings.TrimSpace(provider))}, repository.Hierarchy()...)
	return strings.Join(segments, "/")
}

// BuildSnippetResourcePrefix returns snippets/provider/{provider}/{identifier}
// for a standalone gist or personal snippet, which have no owner/repo
// hierarchy.
func BuildSnippetResourcePrefix(provider, identifier string) string {
	segments := []string{SnippetsRoot, "provider", strings.ToLower(strings.TrimSpace(provider)), SanitizeIdentifier(identifier)}
	return strings.Join(segments, "/")
}

// BuildNestedSnippetPrefix returns {repositoryPrefix}/snippets/{identifier} for
// a snippet nested under its owning repository.
func BuildNestedSnippetPrefix(repositoryPrefix, identifier string) string {
	return strings.Trim(repositoryPrefix, "/") + "/snippets/" + SanitizeIdentifier(identifier)
}

// BuildURLRepositoryPrefix returns repositories/url/{domain}/{hierarchy}.
func BuildURLRepositoryPrefix(repository PathInfo) string {
	segments := append([]string{RepositoriesRoot, "url", repository.FullDomain}, repository.Hierarchy()...)
	return strings.Join(segments, "/")
}

// SanitizeIdentifier defends against a provider-supplied identifier containing
// '/' (or '..') steering this resource's objects under a different key prefix:
// the shared normalizer strips anything outside the safe segment charset.
func SanitizeIdentifier(identifier string) string {
	return NormalizeStorageSegment(identifier, "unknown", false)
}

// BuildArchiveObjectKey returns {repositoryPrefix}/{timestamp}_repo.tar.gz.
func BuildArchiveObjectKey(repositoryPrefix string, timestampUnixSeconds int64) string {
	return strings.Trim(repositoryPrefix, "/") + "/" + strconv.FormatInt(timestampUnixSeconds, 10) + ArchiveObjectNameSuffix
}

// BuildRepositoryMetadataObjectKey returns {repositoryPrefix}/metadata.json.
func BuildRepositoryMetadataObjectKey(repositoryPrefix string) string {
	return strings.Trim(repositoryPrefix, "/") + "/" + RepositoryMetadataObjectName
}

// Issues, merge requests, and releases are stored as latest-state JSON
// documents nested under their repository prefix:
// {repositoryPrefix}/{collection}/{identifier}.json, each collection with an
// index.json manifest and an attachments/{identifier}/ folder for downloaded
// files. The identifier is the issue/MR number for those collections and the
// sanitized tag for releases.

// BuildIssuesCollectionPrefix returns {repositoryPrefix}/issues.
func BuildIssuesCollectionPrefix(repositoryPrefix string) string {
	return buildCollectionPrefix(repositoryPrefix, IssuesCollectionSegment)
}

// BuildIssueObjectKey returns {repositoryPrefix}/issues/{identifier}.json.
func BuildIssueObjectKey(repositoryPrefix, identifier string) string {
	return buildDocumentObjectKey(BuildIssuesCollectionPrefix(repositoryPrefix), identifier)
}

// BuildIssuesManifestObjectKey returns {repositoryPrefix}/issues/index.json.
func BuildIssuesManifestObjectKey(repositoryPrefix string) string {
	return buildManifestObjectKey(BuildIssuesCollectionPrefix(repositoryPrefix))
}

// BuildIssueAttachmentObjectKey returns an attachment key inside the issues
// collection.
func BuildIssueAttachmentObjectKey(repositoryPrefix, identifier, fileName string) string {
	return buildAttachmentObjectKey(BuildIssuesCollectionPrefix(repositoryPrefix), identifier, fileName)
}

// BuildMergeRequestsCollectionPrefix returns {repositoryPrefix}/merge-requests.
func BuildMergeRequestsCollectionPrefix(repositoryPrefix string) string {
	return buildCollectionPrefix(repositoryPrefix, MergeRequestsCollectionSegment)
}

// BuildMergeRequestObjectKey returns
// {repositoryPrefix}/merge-requests/{identifier}.json.
func BuildMergeRequestObjectKey(repositoryPrefix, identifier string) string {
	return buildDocumentObjectKey(BuildMergeRequestsCollectionPrefix(repositoryPrefix), identifier)
}

// BuildMergeRequestsManifestObjectKey returns
// {repositoryPrefix}/merge-requests/index.json.
func BuildMergeRequestsManifestObjectKey(repositoryPrefix string) string {
	return buildManifestObjectKey(BuildMergeRequestsCollectionPrefix(repositoryPrefix))
}

// BuildMergeRequestAttachmentObjectKey returns an attachment key inside the
// merge-requests collection.
func BuildMergeRequestAttachmentObjectKey(repositoryPrefix, identifier, fileName string) string {
	return buildAttachmentObjectKey(BuildMergeRequestsCollectionPrefix(repositoryPrefix), identifier, fileName)
}

// BuildReleasesCollectionPrefix returns {repositoryPrefix}/releases.
func BuildReleasesCollectionPrefix(repositoryPrefix string) string {
	return buildCollectionPrefix(repositoryPrefix, ReleasesCollectionSegment)
}

// BuildReleaseObjectKey returns {repositoryPrefix}/releases/{identifier}.json.
func BuildReleaseObjectKey(repositoryPrefix, identifier string) string {
	return buildDocumentObjectKey(BuildReleasesCollectionPrefix(repositoryPrefix), identifier)
}

// BuildReleasesManifestObjectKey returns
// {repositoryPrefix}/releases/index.json.
func BuildReleasesManifestObjectKey(repositoryPrefix string) string {
	return buildManifestObjectKey(BuildReleasesCollectionPrefix(repositoryPrefix))
}

// BuildReleaseAttachmentObjectKey returns an attachment key inside the releases
// collection.
func BuildReleaseAttachmentObjectKey(repositoryPrefix, identifier, fileName string) string {
	return buildAttachmentObjectKey(BuildReleasesCollectionPrefix(repositoryPrefix), identifier, fileName)
}

func buildCollectionPrefix(repositoryPrefix, collectionSegment string) string {
	return strings.Trim(repositoryPrefix, "/") + "/" + collectionSegment
}

func buildDocumentObjectKey(collectionPrefix, identifier string) string {
	return collectionPrefix + "/" + identifier + ".json"
}

func buildManifestObjectKey(collectionPrefix string) string {
	return collectionPrefix + "/" + CollectionManifestObjectName
}

func buildAttachmentObjectKey(collectionPrefix, identifier, fileName string) string {
	return collectionPrefix + "/" + AttachmentsCollectionSegment + "/" + identifier + "/" + fileName
}

// TryGetArchiveTimestamp parses the snapshot timestamp encoded in an archive
// object key ({prefix}/{unixSeconds}_repo.tar.gz). It returns false for
// non-archive keys.
func TryGetArchiveTimestamp(objectKey string) (int64, bool) {
	if !strings.HasSuffix(objectKey, ArchiveObjectNameSuffix) {
		return 0, false
	}

	leaf := objectKey[strings.LastIndex(objectKey, "/")+1:]
	timestampText := leaf[:len(leaf)-len(ArchiveObjectNameSuffix)]
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || timestamp <= 0 {
		return 0, false
	}
	return timestamp, true
}

// GetParentPrefix returns the parent prefix of an object key (everything before
// the last '/'), which for an archive or metadata object is its repository
// prefix.
func GetParentPrefix(objectKey string) string {
	lastSlash := strings.LastIndex(objectKey, "/")
	if lastSlash <= 0 {
		return ""
	}
	return objectKey[:lastSlash]
}

// EnsurePrefix trims surrounding slashes from keyOrPrefix and appends exactly
// one trailing '/', yielding the listing prefix for a repository prefix. A
// blank input yields an empty string.
func EnsurePrefix(keyOrPrefix string) string {
	value := strings.Trim(keyOrPrefix, "/")
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return value + "/"
}
