package backup

import (
	"encoding/json"
	"time"
)

// repositoryMetadataDocument is the advisory, human-readable metadata written
// alongside a repository's snapshots. It records only the facts that cannot be
// derived from the object keys (the source URL and mode). It is never read
// back for retention decisions — the object listing is the source of truth —
// so it is safe to lose or rebuild.
type repositoryMetadataDocument struct {
	Mode                 string `json:"mode"`
	RepositoryURL        string `json:"repositoryUrl"`
	UpdatedAtUnixSeconds int64  `json:"updatedAtUnixSeconds"`
}

// collectionManifestEntry is one row in a collection's index.json manifest,
// giving a later browser UI a listing without needing to read every document
// or list the bucket. Nullable fields serialize as null to match the stored
// format.
type collectionManifestEntry struct {
	Number    int64      `json:"number"`
	Title     *string    `json:"title"`
	State     *string    `json:"state"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// releaseManifestEntry is one row in a release collection's index.json
// manifest. Releases are keyed by tag rather than a number, so they use a
// distinct manifest shape.
type releaseManifestEntry struct {
	Tag         string     `json:"tag"`
	Name        *string    `json:"name"`
	PublishedAt *time.Time `json:"publishedAt"`
}

// serializeMetadata renders a document as indented camelCase JSON, matching
// the stored format of every metadata document.
func serializeMetadata(document any) (string, error) {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
