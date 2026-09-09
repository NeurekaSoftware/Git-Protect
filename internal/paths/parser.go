package paths

import (
	"fmt"
	"net/url"
	"strings"
)

// PathInfo describes where a repository lives within a forge's URL hierarchy.
type PathInfo struct {
	// FullDomain is the normalized host the repository was cloned from.
	FullDomain string
	// Owner is the first path segment.
	Owner string
	// Group is the second path segment when the URL nests deeper than
	// owner/repository (for example GitLab subgroups).
	Group string
	// SecondaryGroup joins every remaining middle segment with '-'.
	SecondaryGroup string
	// RepositoryName is the last path segment with any .git suffix removed.
	RepositoryName string
}

// Hierarchy returns the non-empty normalized segments between the storage root
// and the repository name, in key order.
func (p PathInfo) Hierarchy() []string {
	segments := []string{p.Owner}
	if p.Group != "" {
		segments = append(segments, p.Group)
	}
	if p.SecondaryGroup != "" {
		segments = append(segments, p.SecondaryGroup)
	}
	return append(segments, p.RepositoryName)
}

// ParseRepositoryPath splits a repository clone URL into the normalized
// hierarchy used to build its storage prefix.
func ParseRepositoryPath(repositoryURL string) (PathInfo, error) {
	parsed, err := url.Parse(strings.TrimSpace(repositoryURL))
	if err != nil || !parsed.IsAbs() {
		return PathInfo{}, fmt.Errorf("invalid repository URL '%s'.", repositoryURL)
	}

	if !IsHTTPOrHTTPS(parsed) {
		return PathInfo{}, fmt.Errorf("unsupported repository URL scheme in '%s'. Only http and https are supported.", repositoryURL)
	}

	segments := SplitUnescapedSegments(parsed)
	if len(segments) < 2 {
		return PathInfo{}, fmt.Errorf("repository URL '%s' does not contain owner and repository segments.", repositoryURL)
	}

	info := PathInfo{
		FullDomain:     normalizeSegment(parsed.Hostname()),
		Owner:          normalizeSegment(segments[0]),
		RepositoryName: normalizeSegment(TrimGitSuffix(segments[len(segments)-1])),
	}

	groupSegments := segments[1 : len(segments)-1]
	if len(groupSegments) > 0 {
		info.Group = normalizeSegment(groupSegments[0])
	}
	if len(groupSegments) > 1 {
		info.SecondaryGroup = normalizeSegment(strings.Join(groupSegments[1:], "-"))
	}

	return info, nil
}

func normalizeSegment(value string) string {
	return NormalizeStorageSegment(value, "unknown", true)
}
