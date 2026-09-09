// Package backup orchestrates a sync run: it mirrors repositories from a
// forge or a URL list to local disk, uploads timestamped tar.gz snapshots to
// object storage, backs up project metadata (issues, merge requests,
// releases), and prunes expired snapshots.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
)

// MirrorStore manages the on-disk git mirrors under
// {workingRoot}/repositories. Each repository maps to a single flat directory
// named by a deterministic hash of its storage prefix, so cleaning up mirrors
// for repositories that are no longer backed up is a simple set difference.
type MirrorStore struct {
	mirrorsRoot string
}

// NewMirrorStore roots the mirror cache under the working root.
func NewMirrorStore(workingRoot string) *MirrorStore {
	return &MirrorStore{mirrorsRoot: filepath.Join(workingRoot, "repositories")}
}

// GetMirrorPath is the local path of one repository's bare mirror.
func (m *MirrorStore) GetMirrorPath(repositoryPrefix string) string {
	return filepath.Join(m.mirrorsRoot, GetMirrorDirectoryName(repositoryPrefix))
}

// GetMirrorDirectoryName derives the flat, deterministic directory name for a
// storage prefix: the lowercase hex SHA-256 of the prefix.
func GetMirrorDirectoryName(repositoryPrefix string) string {
	sum := sha256.Sum256([]byte(repositoryPrefix))
	return hex.EncodeToString(sum[:])
}

// TryDeleteMirror removes one repository's mirror, tolerating absence.
func (m *MirrorStore) TryDeleteMirror(repositoryPrefix string) {
	tryDeleteDirectory(m.GetMirrorPath(repositoryPrefix))
}

// RemoveOrphans deletes any mirror directory that is not in
// expectedDirectoryNames — i.e. belongs to a repository that is no longer
// being backed up. Callers must only pass a complete expected set, otherwise a
// transient discovery error could remove a valid mirror.
func (m *MirrorStore) RemoveOrphans(expectedDirectoryNames map[string]struct{}) {
	entries, err := os.ReadDir(m.mirrorsRoot)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, expected := expectedDirectoryNames[entry.Name()]; expected {
			continue
		}

		directory := filepath.Join(m.mirrorsRoot, entry.Name())
		if tryDeleteDirectory(directory) {
			slog.Info("Removed local mirror for a repository that is no longer backed up.", "path", directory)
		}
	}
}

func tryDeleteDirectory(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("Failed to remove local mirror directory.", "path", path, "error", err.Error())
		return false
	}
	return true
}
