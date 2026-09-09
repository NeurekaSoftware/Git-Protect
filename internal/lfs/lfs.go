// Package lfs fetches a repository's Git LFS objects for all refs into the
// standard local cache layout, mirroring what `git lfs fetch --all` does for
// the git command-line suite.
//
// The flow is the LFS batch protocol: scan every ref's trees for pointer
// blobs, POST them to the endpoint's objects/batch action, then stream each
// object's download into .git/lfs/objects (or lfs/objects for bare
// repositories) while verifying its SHA-256.
package lfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
)

// ErrDisabled reports that the remote has Git LFS turned off entirely, which is
// an expected state rather than a backup failure.
var ErrDisabled = errors.New("git lfs is disabled on the remote")

// lfsConfigMaxBytes bounds how much of a .lfsconfig file is read; real
// configuration files are a few hundred bytes.
const lfsConfigMaxBytes = 64 * 1024

// Fetcher downloads LFS objects into a local repository's cache.
type Fetcher struct {
	client *batchClient
}

// NewFetcher returns an LFS fetcher using the default HTTP client.
func NewFetcher() *Fetcher {
	return &Fetcher{client: newBatchClient(nil)}
}

// FetchAll fetches every LFS object reachable from the repository's refs at
// repositoryPath, using remoteURL to derive the LFS endpoint. username and
// password authenticate the batch request only; object downloads use the
// server-provided (typically pre-signed) URLs.
func (f *Fetcher) FetchAll(ctx context.Context, repositoryPath, remoteURL, username, password string) error {
	repository, err := git.PlainOpen(repositoryPath)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	endpoint, err := resolveEndpoint(repository, remoteURL)
	if err != nil {
		return err
	}

	pointers, err := collectPointers(ctx, repository)
	if err != nil {
		return fmt.Errorf("scan for LFS pointers: %w", err)
	}
	if len(pointers) == 0 {
		// Nothing to fetch; never contact the endpoint so a forge without LFS
		// is not mistaken for one that disabled it.
		return nil
	}

	objects, err := f.client.batch(ctx, endpoint, username, password, pointers)
	if err != nil {
		return err
	}

	store := objectStoreDir(repository, repositoryPath)
	return downloadObjects(ctx, f.client, store, endpoint, username, password, objects)
}

// resolveEndpoint determines the LFS API root: an lfs.url override from the
// repository's committed .lfsconfig when present, otherwise the remote URL's
// standard /info/lfs root. Reading .lfsconfig is best-effort — any failure
// falls back to the derived endpoint, matching the common deployment.
func resolveEndpoint(repository *git.Repository, remoteURL string) (string, error) {
	remote := strings.TrimSuffix(remoteURL, "/")
	if remote == "" {
		return "", fmt.Errorf("empty remote URL")
	}
	endpoint := remote + "/info/lfs"

	config, err := readLFSConfig(repository)
	if err != nil || config == nil {
		return endpoint, nil
	}
	if override := config.Raw.Section("lfs").Option("url"); override != "" {
		return strings.TrimSuffix(override, "/"), nil
	}
	return endpoint, nil
}

func readLFSConfig(repository *git.Repository) (*config.Config, error) {
	head, err := repository.Head()
	if err != nil {
		return nil, err
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	file, err := tree.File(".lfsconfig")
	if err != nil {
		return nil, err
	}
	reader, err := file.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	content, err := io.ReadAll(io.LimitReader(reader, lfsConfigMaxBytes))
	if err != nil {
		return nil, err
	}
	return config.ReadConfig(bytes.NewReader(content))
}

// objectStoreDir mirrors git-lfs' cache location: a bare repository stores LFS
// objects under its own root, a working copy under .git/lfs.
func objectStoreDir(repository *git.Repository, repositoryPath string) string {
	config, err := repository.Config()
	if err == nil && !config.Core.IsBare {
		return filepath.Join(repositoryPath, ".git", "lfs", "objects")
	}
	return filepath.Join(repositoryPath, "lfs", "objects")
}
