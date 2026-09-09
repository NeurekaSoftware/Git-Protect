package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/forge"
	"github.com/neurekadev/git-backup/internal/git"
)

// --- fakes ---

type fakeGit struct {
	mu    sync.Mutex
	calls []string
	errs  map[string]error
}

func (f *fakeGit) SyncBareRepository(_ context.Context, remoteURL, localPath string, credential *git.Credential, cache, includeLFS bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, remoteURL)
	if f.errs == nil {
		return nil
	}
	if err, scheduled := f.errs[remoteURL]; scheduled {
		return err
	}
	return nil
}

type fakeStorage struct {
	mu      sync.Mutex
	objects map[string]string
	tars    map[string]string // object key → source directory
	deleted [][]string
	listErr error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: make(map[string]string), tars: make(map[string]string)}
}

func (f *fakeStorage) UploadDirectoryAsTarGz(_ context.Context, localDirectory, objectKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tars[objectKey] = localDirectory
	f.objects[objectKey] = "tar"
	return nil
}

func (f *fakeStorage) UploadText(_ context.Context, objectKey, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[objectKey] = content
	return nil
}

func (f *fakeStorage) UploadStream(_ context.Context, objectKey string, _ io.Reader, contentType string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[objectKey] = "stream:" + contentType
	return nil
}

func (f *fakeStorage) ListObjectKeys(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeStorage) DeleteObjects(_ context.Context, objectKeys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range objectKeys {
		delete(f.objects, key)
	}
	f.deleted = append(f.deleted, objectKeys)
	return nil
}

func (f *fakeStorage) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

// fakeProvider implements the forge provider + metadata interfaces.
type fakeProvider struct {
	providerName string
	snippets     bool
	repositories []forge.DiscoveredRepository
	issues       []forge.Issue
	releases     []forge.Release
	listErr      error
	openErr      error
}

func (f *fakeProvider) Provider() string       { return f.providerName }
func (f *fakeProvider) SupportsSnippets() bool { return f.snippets }

func (f *fakeProvider) ListRepositories(_ context.Context, options forge.RepositoryJobOptions, _ *forge.Credential) ([]forge.DiscoveredRepository, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var result []forge.DiscoveredRepository
	for _, repository := range f.repositories {
		if options.IncludeStarred || !repository.IsStarred {
			result = append(result, repository)
		}
	}
	return result, nil
}

func (f *fakeProvider) SupportsIssues() bool        { return true }
func (f *fakeProvider) SupportsMergeRequests() bool { return true }
func (f *fakeProvider) SupportsReleases() bool      { return true }
func (f *fakeProvider) SupportsArtifacts() bool     { return true }

func (f *fakeProvider) ListIssues(_ context.Context, _ *forge.MetadataContext, _ *forge.Credential, deliver func(forge.Issue) error) error {
	if f.listErr != nil {
		return f.listErr
	}
	for _, issue := range f.issues {
		if err := deliver(issue); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeProvider) ListMergeRequests(_ context.Context, _ *forge.MetadataContext, _ *forge.Credential, deliver func(forge.MergeRequest) error) error {
	return nil
}

func (f *fakeProvider) ListReleases(_ context.Context, _ *forge.MetadataContext, _ *forge.Credential, deliver func(forge.Release) error) error {
	for _, release := range f.releases {
		if err := deliver(release); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeProvider) OpenAttachment(_ context.Context, _ *forge.MetadataContext, _ *forge.Credential, _ string) (*forge.AttachmentStream, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return forge.NewAttachmentStream(io.NopCloser(strings.NewReader("attachment-bytes")), 1<<30, 16), nil
}

// --- helpers ---

func testSettings() *config.Settings {
	return &config.Settings{
		Storage: config.Storage{
			Endpoint: "http://127.0.0.1:9000", Region: "r", AccessKeyID: "k",
			SecretAccessKey: "s", Bucket: "b", RetentionMinimum: 1,
			PayloadSignatureMode: config.SignatureFull,
		},
		Credentials: map[string]*config.Credential{
			"token": {Name: "token", Username: "octo", APIKey: "key-123"},
		},
		Concurrency: config.Concurrency{Repositories: 1, Metadata: 1},
		Health:      config.Health{Port: 8080, Bind: "localhost"},
	}
}

func newSyncService(t *testing.T, gitService GitRepositoryService, storage ObjectStorage, providers ...forge.RepositoryProviderClient) (*RepositorySyncService, *MetadataSyncService) {
	t.Helper()
	// Always register the three supported providers so factory validation
	// passes; tests override individual ones by name (first wins).
	all := []forge.RepositoryProviderClient{
		&fakeProvider{providerName: "github"},
		&fakeProvider{providerName: "gitlab"},
		&fakeProvider{providerName: "forgejo"},
	}
	all = append(all, providers...)
	factory, err := forge.NewFactory(all...)
	if err != nil {
		t.Fatal(err)
	}
	mirrorStore := NewMirrorStore(t.TempDir())
	metadataSync := NewMetadataSyncService(factory)
	syncService := NewRepositorySyncService(factory, gitService,
		func(*config.Settings) (ObjectStorage, error) { return storage, nil },
		mirrorStore, metadataSync)
	return syncService, metadataSync
}

// --- mirror store ---

func TestMirrorStorePathsAreDeterministic(t *testing.T) {
	store := NewMirrorStore("/data")
	first := store.GetMirrorPath("repositories/provider/github/octo/repo")
	second := store.GetMirrorPath("repositories/provider/github/octo/repo")
	if first != second {
		t.Fatal("mirror paths should be deterministic")
	}
	if filepath.Base(first) != GetMirrorDirectoryName("repositories/provider/github/octo/repo") {
		t.Error("directory name should be the prefix hash")
	}
	if len(filepath.Base(first)) != 64 || strings.ToLower(filepath.Base(first)) != filepath.Base(first) {
		t.Errorf("directory name should be lowercase hex sha256: %q", filepath.Base(first))
	}
}

func TestMirrorStoreRemoveOrphans(t *testing.T) {
	root := t.TempDir()
	store := NewMirrorStore(root)

	keep := store.GetMirrorPath("repositories/provider/github/octo/keep")
	drop := store.GetMirrorPath("repositories/provider/github/octo/drop")
	for _, path := range []string{keep, drop} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store.RemoveOrphans(map[string]struct{}{
		GetMirrorDirectoryName("repositories/provider/github/octo/keep"): {},
	})

	if _, err := os.Stat(keep); err != nil {
		t.Error("the expected mirror should survive")
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Error("the orphaned mirror should be removed")
	}
}

// --- URL mode ---

func urlJob(urls ...string) *config.Repository {
	return &config.Repository{Mode: config.ModeURL, Urls: urls, Enabled: true, LFS: true, Cache: true}
}

func TestRunURLModeSnapshotsEachRepository(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	syncService, _ := newSyncService(t, gitService, storage)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		urlJob("https://example.com/octo/one.git", "https://example.com/octo/two.git"),
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if len(gitService.calls) != 2 {
		t.Fatalf("git calls = %v", gitService.calls)
	}
	if len(storage.tars) != 2 {
		t.Fatalf("archive uploads = %d, want 2", len(storage.tars))
	}
	for key := range storage.tars {
		if !strings.HasSuffix(key, "_repo.tar.gz") {
			t.Errorf("archive key %q should carry the unix-timestamp suffix", key)
		}
		prefix := key[:strings.LastIndex(key, "/")]
		if !storage.has(prefix + "/metadata.json") {
			t.Errorf("metadata.json missing for %q", prefix)
		}
	}
}

func TestRunURLModeDeduplicatesCaseInsensitively(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	syncService, _ := newSyncService(t, gitService, storage)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		urlJob("https://example.com/octo/one.git", "https://example.com/octo/ONE.git"),
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if len(gitService.calls) != 1 {
		t.Fatalf("git calls = %v, want one (case-variant duplicate dropped)", gitService.calls)
	}
}

func TestRunSkipsInaccessibleRemote(t *testing.T) {
	gitService := &fakeGit{errs: map[string]error{
		"https://example.com/octo/private.git": &git.RemoteInaccessibleError{Err: errors.New("authentication required")},
	}}
	storage := newFakeStorage()
	syncService, _ := newSyncService(t, gitService, storage)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		urlJob("https://example.com/octo/private.git", "https://example.com/octo/ok.git"),
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if len(storage.tars) != 1 {
		t.Fatalf("archive uploads = %d, want 1 (the inaccessible one skipped)", len(storage.tars))
	}
}

func TestRunIncompletePictureSkipsMirrorCleanup(t *testing.T) {
	// A provider discovery failure means the full repository set is unknown,
	// so this run's picture is incomplete and mirror cleanup must be skipped.
	gitService := &fakeGit{}
	storage := newFakeStorage()
	mirrorStore := NewMirrorStore(t.TempDir())
	provider := &fakeProvider{
		providerName: "github",
		listErr:      errors.New("provider unavailable"),
	}
	syncService, _ := newSyncService(t, gitService, storage, provider)

	// Pre-existing mirror for a repository that is no longer configured: it
	// may only be removed when this run's picture is complete.
	orphan := mirrorStore.GetMirrorPath("repositories/url/example.com/octo/old")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeProvider, Provider: "github", Credential: "token", Enabled: true,
			LFS: true, Cache: true},
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("an incomplete run must not delete local mirrors")
	}
}

func mustFactory(t *testing.T) *forge.Factory {
	t.Helper()
	factory, err := forge.NewFactory(
		&fakeProvider{providerName: "github"},
		&fakeProvider{providerName: "gitlab"},
		&fakeProvider{providerName: "forgejo"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

// --- provider mode + metadata ---

func TestRunProviderModeSyncsAndBacksUpMetadata(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	provider := &fakeProvider{
		providerName: "github",
		repositories: []forge.DiscoveredRepository{
			{CloneURL: "https://github.com/octo/owned.git", WebURL: "https://github.com/octo/owned"},
			{CloneURL: "https://github.com/octo/starred.git", IsStarred: true},
			{CloneURL: "https://gist.github.com/abc.git", Kind: forge.KindGist, Identifier: "abc"},
		},
		issues: []forge.Issue{
			{Number: 1, Title: "first"},
		},
	}
	syncService, _ := newSyncService(t, gitService, storage, provider)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeProvider, Provider: "github", Credential: "token", Enabled: true,
			LFS: true, Cache: true, IncludeStarred: true, IncludeIssues: true, IncludeIssueArtifacts: true},
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	// Three resources mirrored: the owned repo, the starred repo, and the gist
	// under the snippets root.
	if len(gitService.calls) != 3 {
		t.Fatalf("git calls = %v", gitService.calls)
	}
	foundGist := false
	for key := range storage.tars {
		if strings.HasPrefix(key, "snippets/provider/github/abc/") {
			foundGist = true
		}
	}
	if !foundGist {
		t.Error("the gist should be stored under the snippets root")
	}

	// Issues are backed up only for the owned repository.
	if !storage.has("repositories/provider/github/octo/owned/issues/1.json") {
		t.Error("the owned repository's issue should be backed up")
	}
	if storage.has("repositories/provider/github/octo/starred/issues/1.json") {
		t.Error("a starred repository must not have its issues backed up")
	}
	if !storage.has("repositories/provider/github/octo/owned/issues/index.json") {
		t.Error("the issues manifest is missing")
	}
}

func TestMetadataReconcileDeletesRemovedItems(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	prefix := "repositories/provider/github/octo/owned"
	storage.objects[prefix+"/issues/1.json"] = "old"
	storage.objects[prefix+"/issues/2.json"] = "old"
	storage.objects[prefix+"/issues/index.json"] = "old manifest"
	storage.objects[prefix+"/issues/attachments/2/file.png"] = "old attachment"

	provider := &fakeProvider{
		providerName: "github",
		repositories: []forge.DiscoveredRepository{
			{CloneURL: "https://github.com/octo/owned.git", WebURL: "https://github.com/octo/owned"},
		},
		// Issue 1 still exists upstream; issue 2 and its attachments are gone.
		issues: []forge.Issue{{Number: 1, Title: "first"}},
	}
	syncService, _ := newSyncService(t, gitService, storage, provider)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeProvider, Provider: "github", Credential: "token", Enabled: true,
			LFS: true, Cache: true, IncludeIssues: true},
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if !storage.has(prefix + "/issues/1.json") {
		t.Error("the still-existing issue document must survive")
	}
	if !storage.has(prefix + "/issues/index.json") {
		t.Error("the manifest must survive reconcile")
	}
	if storage.has(prefix + "/issues/2.json") {
		t.Error("the removed issue document should be deleted")
	}
	if storage.has(prefix + "/issues/attachments/2/file.png") {
		t.Error("the removed issue's attachment subtree should be deleted")
	}
	if !storage.has(prefix + "/metadata.json") {
		t.Error("objects outside the collection prefix must survive reconcile")
	}
}

func TestMetadataListingFailureKeepsEverything(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	prefix := "repositories/provider/github/octo/owned"
	storage.objects[prefix+"/issues/1.json"] = "old"
	storage.objects[prefix+"/issues/index.json"] = "old manifest"

	provider := &fakeProvider{
		providerName: "github",
		repositories: []forge.DiscoveredRepository{
			{CloneURL: "https://github.com/octo/owned.git"},
		},
		listErr: errors.New("rate limited"),
	}
	syncService, _ := newSyncService(t, gitService, storage, provider)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeProvider, Provider: "github", Credential: "token", Enabled: true,
			LFS: true, Cache: true, IncludeIssues: true},
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if !storage.has(prefix+"/issues/1.json") || !storage.has(prefix+"/issues/index.json") {
		t.Error("a failed listing must leave stored items and the manifest untouched")
	}
	if len(storage.deleted) != 0 {
		t.Errorf("deletes = %v, want none", storage.deleted)
	}
}

func TestMetadataAttachmentDownloadRecordsStorageKey(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	provider := &fakeProvider{
		providerName: "github",
		repositories: []forge.DiscoveredRepository{
			{CloneURL: "https://github.com/octo/owned.git", WebURL: "https://github.com/octo/owned"},
		},
		issues: []forge.Issue{{
			Number: 1, Title: "with attachment",
			Attachments: []forge.Attachment{{
				FileName:     "img.png",
				OriginalPath: "https://user-images.githubusercontent.com/u/1/img.png",
				DownloadURL:  "https://user-images.githubusercontent.com/u/1/img.png",
				Downloadable: true,
			}},
		}},
	}
	syncService, _ := newSyncService(t, gitService, storage, provider)

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeProvider, Provider: "github", Credential: "token", Enabled: true,
			LFS: true, Cache: true, IncludeIssues: true, IncludeIssueArtifacts: true},
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	prefix := "repositories/provider/github/octo/owned"
	if !storage.has(prefix + "/issues/attachments/1/img.png") {
		t.Error("the attachment should be uploaded under the collection's attachments subtree")
	}
}

func TestReleaseSlugGuardsIndexCollision(t *testing.T) {
	if got := resolveReleaseSlug("v1.0"); got != "v1.0" {
		t.Errorf("resolveReleaseSlug = %q", got)
	}
	slug := resolveReleaseSlug("index")
	if slug == "index" {
		t.Fatal("a tag named index must not collide with index.json")
	}
	if !strings.HasPrefix(slug, "index-") {
		t.Errorf("colliding slug = %q, want an index-<hash> suffix", slug)
	}
}

func TestNonCachedMirrorDeletedAfterUpload(t *testing.T) {
	gitService := &fakeGit{}
	storage := newFakeStorage()
	mirrorStore := NewMirrorStore(t.TempDir())
	syncService := NewRepositorySyncService(mustFactory(t), gitService,
		func(*config.Settings) (ObjectStorage, error) { return storage, nil },
		mirrorStore, NewMetadataSyncService(mustFactory(t)))

	settings := testSettings()
	settings.Repositories = []*config.Repository{
		{Mode: config.ModeURL, Urls: []string{"https://example.com/octo/one.git"}, Enabled: true,
			LFS: true, Cache: false},
	}

	// Simulate the mirror the (faked) git step would have produced.
	mirror := mirrorStore.GetMirrorPath("repositories/url/example.com/octo/one")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := syncService.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	// A cache:false run deletes the mirror once its snapshot uploaded.
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Error("the non-cached mirror should be deleted after a successful upload")
	}
}
