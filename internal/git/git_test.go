package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// sourceURL is the in-process remote the tests clone from; TestMain routes the
// http scheme to an embedded go-git server so the full mirror flow is
// exercised without any external binary or network.
const sourceURL = "http://gitbackup.test/source.git"

var testLoader = &armedLoader{repositories: make(map[string]storer.Storer)}

// armedLoader serves the registered repositories but can be told to fail, so
// tests can exercise the incremental-fetch failure and self-heal paths.
type armedLoader struct {
	repositories map[string]storer.Storer
	fail         atomic.Bool
}

func (l *armedLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	if l.fail.Load() {
		return nil, errors.New("server unavailable")
	}
	repository, ok := l.repositories[ep.String()]
	if !ok {
		return nil, transport.ErrRepositoryNotFound
	}
	return repository, nil
}

func TestMain(m *testing.M) {
	client.InstallProtocol("http", server.NewClient(testLoader))
	os.Exit(m.Run())
}

// createSourceRepository builds a working-copy repository with one commit and
// serves it at sourceURL.
func createSourceRepository(t *testing.T) (*git.Repository, string) {
	t.Helper()
	return createSourceRepositoryAt(t, sourceURL)
}

func createSourceRepositoryAt(t *testing.T, remoteURL string) (*git.Repository, string) {
	t.Helper()

	dir := t.TempDir()
	repository, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addFile(repository, dir, "README.md", "hello\n"); err != nil {
		t.Fatal(err)
	}

	storer := filesystem.NewStorage(osfs.New(filepath.Join(dir, ".git")), cache.NewObjectLRUDefault())
	endpoint, err := transport.NewEndpoint(remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	testLoader.repositories[endpoint.String()] = storer
	return repository, dir
}

func addFile(repository *git.Repository, dir, name, content string) (plumbing.Hash, error) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return plumbing.ZeroHash, err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := worktree.Add(name); err != nil {
		return plumbing.ZeroHash, err
	}
	return worktree.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
}

func createBranch(t *testing.T, repository *git.Repository, name string) {
	t.Helper()
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	err = repository.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), head.Hash()))
	if err != nil {
		t.Fatal(err)
	}
}

func deleteBranch(t *testing.T, repository *git.Repository, name string) {
	t.Helper()
	if err := repository.Storer.RemoveReference(plumbing.NewBranchReferenceName(name)); err != nil {
		t.Fatal(err)
	}
}

func mirrorHasCommit(t *testing.T, mirrorPath string, hash plumbing.Hash) bool {
	t.Helper()
	repository, err := git.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	_, err = repository.CommitObject(hash)
	return err == nil
}

func TestSyncBareRepositoryRejectsUnsupportedTransport(t *testing.T) {
	service := NewRepositoryService()
	for _, remoteURL := range []string{"file:///tmp/repo", "ssh://git@example.com/repo.git", "ext::sh -c cat", "/local/path", "not a url"} {
		err := service.SyncBareRepository(context.Background(), remoteURL, filepath.Join(t.TempDir(), "mirror"), nil, false, false)
		if err == nil {
			t.Errorf("SyncBareRepository(%q) should reject non-http transports", remoteURL)
			continue
		}
		if !strings.Contains(err.Error(), "Only http and https clone URLs are allowed") {
			t.Errorf("SyncBareRepository(%q) error = %v", remoteURL, err)
		}
	}
}

func TestSyncBareRepositoryFreshClone(t *testing.T) {
	source, _ := createSourceRepository(t)
	head, err := source.Head()
	if err != nil {
		t.Fatal(err)
	}

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, false, false); err != nil {
		t.Fatalf("fresh clone failed: %v", err)
	}

	if !isBareRepository(mirrorPath) {
		t.Error("mirror should be a bare repository")
	}
	if !mirrorHasCommit(t, mirrorPath, head.Hash()) {
		t.Error("mirror should contain the source commit")
	}
}

func TestSyncBareRepositoryCachedFetchPicksUpNewCommits(t *testing.T) {
	source, dir := createSourceRepository(t)
	head, _ := source.Head()

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	newHash, err := addFile(source, dir, "more.txt", "more\n")
	if err != nil {
		t.Fatal(err)
	}

	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("cached fetch failed: %v", err)
	}
	if !mirrorHasCommit(t, mirrorPath, newHash) {
		t.Error("mirror should contain the new commit after the cached fetch")
	}
	if !mirrorHasCommit(t, mirrorPath, head.Hash()) {
		t.Error("mirror should still contain the original commit")
	}
}

func TestSyncBareRepositoryPrunesDeletedRefs(t *testing.T) {
	source, _ := createSourceRepository(t)
	createBranch(t, source, "feature")

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	if !mirrorHasBranch(t, mirrorPath, "feature") {
		t.Fatal("mirror should contain the feature branch after the first sync")
	}

	deleteBranch(t, source, "feature")
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("pruning fetch failed: %v", err)
	}
	if mirrorHasBranch(t, mirrorPath, "feature") {
		t.Error("mirror should no longer contain the deleted feature branch")
	}
}

func TestSyncBareRepositorySelfHealsAfterFailedFetch(t *testing.T) {
	source, _ := createSourceRepository(t)

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// A failing remote makes the incremental fetch fail; the sync must
	// re-clone from scratch instead of getting stuck.
	testLoader.fail.Store(true)
	err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false)
	if err == nil {
		t.Fatal("sync should fail while the remote is unavailable")
	}

	testLoader.fail.Store(false)
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("sync after recovery failed: %v", err)
	}
	head, _ := source.Head()
	if !mirrorHasCommit(t, mirrorPath, head.Hash()) {
		t.Error("recovered mirror should contain the source commit")
	}
}

func TestSyncBareRepositoryUpdatesOriginURL(t *testing.T) {
	createSourceRepository(t)
	otherURL := "http://gitbackup.test/other.git"
	other, _ := createSourceRepositoryAt(t, otherURL)
	otherHead, _ := other.Head()

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Point the job at a different remote; the cached mirror must follow it.
	if err := service.SyncBareRepository(context.Background(), otherURL, mirrorPath, nil, true, false); err != nil {
		t.Fatalf("re-targeted sync failed: %v", err)
	}
	repository, err := git.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := repository.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.Config().URLs) != 1 || remote.Config().URLs[0] != otherURL {
		t.Errorf("origin URL = %v, want [%s]", remote.Config().URLs, otherURL)
	}
	if !mirrorHasCommit(t, mirrorPath, otherHead.Hash()) {
		t.Error("mirror should contain the new remote's commit")
	}
}

func TestSyncBareRepositoryAcceptsCredential(t *testing.T) {
	source, _ := createSourceRepository(t)
	head, _ := source.Head()

	mirrorPath := filepath.Join(t.TempDir(), "repositories", "mirror")
	service := NewRepositoryService()
	credential := &Credential{Username: "git", Password: "token"}
	if err := service.SyncBareRepository(context.Background(), sourceURL, mirrorPath, credential, false, false); err != nil {
		t.Fatalf("clone with credential failed: %v", err)
	}
	if !mirrorHasCommit(t, mirrorPath, head.Hash()) {
		t.Error("mirror should contain the source commit")
	}
}

func mirrorHasBranch(t *testing.T, mirrorPath, name string) bool {
	t.Helper()
	repository, err := git.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	_, err = repository.Reference(plumbing.NewBranchReferenceName(name), true)
	return err == nil
}

func TestIsBareRepository(t *testing.T) {
	if isBareRepository(filepath.Join(t.TempDir(), "missing")) {
		t.Error("missing path should not be a mirror")
	}

	empty := t.TempDir()
	if isBareRepository(empty) {
		t.Error("empty directory should not be a mirror")
	}

	working, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = working
	if isBareRepository(t.TempDir()) {
		t.Error("freshly initialized empty directory should not be a mirror")
	}

	bareDir := filepath.Join(t.TempDir(), "repo.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatal(err)
	}
	if !isBareRepository(bareDir) {
		t.Error("bare repository should be recognized as a mirror")
	}
}

func TestClassify(t *testing.T) {
	for _, sentinel := range []error{transport.ErrAuthenticationRequired, transport.ErrAuthorizationFailed, transport.ErrRepositoryNotFound} {
		var inaccessible *RemoteInaccessibleError
		if !errors.As(classify(sentinel), &inaccessible) {
			t.Errorf("classify(%v) should be inaccessible", sentinel)
		}
	}

	wrapped := fmt.Errorf("clone: %w", transport.ErrRepositoryNotFound)
	var inaccessible *RemoteInaccessibleError
	if !errors.As(classify(wrapped), &inaccessible) {
		t.Error("classify should see through wrapping")
	}

	plain := errors.New("connection refused")
	if errors.As(classify(plain), &inaccessible) {
		t.Error("genuine failures must not be marked inaccessible")
	}
}
