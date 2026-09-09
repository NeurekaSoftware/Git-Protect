package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/forge"
	"github.com/neurekadev/git-backup/internal/git"
	"github.com/neurekadev/git-backup/internal/paths"
)

// ObjectStorage is the storage surface the sync and retention services need.
type ObjectStorage interface {
	UploadDirectoryAsTarGz(ctx context.Context, localDirectory, objectKey string) error
	UploadText(ctx context.Context, objectKey, content string) error
	UploadStream(ctx context.Context, objectKey string, content io.Reader, contentType string, knownLength int64) error
	ListObjectKeys(ctx context.Context, prefix string) ([]string, error)
	DeleteObjects(ctx context.Context, objectKeys []string) error
}

// GitRepositoryService maintains the local bare mirrors.
type GitRepositoryService interface {
	SyncBareRepository(ctx context.Context, remoteURL, localPath string, credential *git.Credential, cache, includeLFS bool) error
}

// StorageFactory builds one storage client per run from the live settings.
type StorageFactory func(settings *config.Settings) (ObjectStorage, error)

// runCounters aggregates one repository job mode's outcome.
type runCounters struct {
	synced              int64
	skippedInaccessible int64
	complete            bool
}

// RepositorySyncService runs one scheduled repository backup pass.
type RepositorySyncService struct {
	providerFactory   *forge.Factory
	gitRepository     GitRepositoryService
	storageFactory    StorageFactory
	mirrorStore       *MirrorStore
	metadataSync      *MetadataSyncService
	expectedMirrorsMu sync.Mutex
}

// NewRepositorySyncService wires the sync service.
func NewRepositorySyncService(
	providerFactory *forge.Factory,
	gitRepository GitRepositoryService,
	storageFactory StorageFactory,
	mirrorStore *MirrorStore,
	metadataSync *MetadataSyncService,
) *RepositorySyncService {
	return &RepositorySyncService{
		providerFactory: providerFactory,
		gitRepository:   gitRepository,
		storageFactory:  storageFactory,
		mirrorStore:     mirrorStore,
		metadataSync:    metadataSync,
	}
}

// Run executes one repository backup pass over every enabled job.
func (s *RepositorySyncService) Run(ctx context.Context, settings *config.Settings) error {
	enabledRepositories := make([]*config.Repository, 0, len(settings.Repositories))
	for _, repository := range settings.Repositories {
		if repository == nil || repository.Enabled {
			enabledRepositories = append(enabledRepositories, repository)
		}
	}
	slog.Info("Repository run started.", "enabledJobs", len(enabledRepositories))

	objectStorage, err := s.storageFactory(settings)
	if err != nil {
		return err
	}

	slog.Debug("Repository storage target configured.",
		"endpoint", settings.Storage.Endpoint,
		"bucket", settings.Storage.Bucket,
		"region", settings.Storage.Region)

	// Track every repository's mirror directory this run, plus whether the
	// picture is complete. Local-mirror cleanup only runs when complete, so a
	// discovery error never deletes a valid mirror.
	expectedMirrorDirectories := make(map[string]struct{})
	pictureComplete := true
	var syncedRepositories, skippedInaccessible int64

	repositoryConcurrency := max(settings.Concurrency.Repositories, 1)
	metadataConcurrency := max(settings.Concurrency.Metadata, 1)

	// Shared across every repository this run, so the peak number of
	// attachments buffered in memory is capped at metadataConcurrency rather
	// than repositoryConcurrency x metadataConcurrency.
	downloadThrottle := forge.NewThrottle(metadataConcurrency)

	for _, repository := range enabledRepositories {
		if err := ctx.Err(); err != nil {
			return err
		}

		if repository == nil {
			slog.Warn("Skipping repository job because the entry is missing.")
			pictureComplete = false
			continue
		}

		var counters *runCounters
		var runErr error
		switch repository.Mode {
		case config.ModeProvider:
			counters, runErr = s.runProviderMode(ctx, settings, repository, objectStorage,
				expectedMirrorDirectories, repositoryConcurrency, metadataConcurrency, downloadThrottle)
		case config.ModeURL:
			counters, runErr = s.runURLMode(ctx, settings, repository, objectStorage,
				expectedMirrorDirectories, repositoryConcurrency)
		default:
			slog.Warn("Skipping repository job because mode is invalid.", "mode", repository.Mode)
			pictureComplete = false
			continue
		}

		if runErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("Repository job failed.", "mode", repository.Mode, "error", runErr.Error())
			pictureComplete = false
			continue
		}

		syncedRepositories += counters.synced
		skippedInaccessible += counters.skippedInaccessible
		pictureComplete = pictureComplete && counters.complete
	}

	if pictureComplete {
		s.mirrorStore.RemoveOrphans(expectedMirrorDirectories)
	} else {
		slog.Info("Skipping local mirror cleanup because the repository set for this run is incomplete.")
	}

	slog.Info("Repository run completed.",
		"syncedRepositories", syncedRepositories,
		"skippedInaccessible", skippedInaccessible)
	return nil
}

func (s *RepositorySyncService) runProviderMode(
	ctx context.Context,
	settings *config.Settings,
	repository *config.Repository,
	objectStorage ObjectStorage,
	expectedMirrorDirectories map[string]struct{},
	repositoryConcurrency, metadataConcurrency int,
	downloadThrottle forge.Throttle,
) (*runCounters, error) {
	if strings.TrimSpace(repository.Provider) == "" || strings.TrimSpace(repository.Credential) == "" {
		slog.Warn("Skipping provider repository job because provider or credential is missing.")
		return &runCounters{}, nil
	}

	credentialConfig, known := settings.Credentials[strings.ToLower(repository.Credential)]
	if !known {
		slog.Warn("Skipping provider repository job because credential is missing.",
			"provider", repository.Provider, "credential", repository.Credential)
		return &runCounters{}, nil
	}

	slog.Info("Provider repository discovery started.", "provider", repository.Provider)
	providerClient, err := s.providerFactory.Resolve(repository.Provider)
	if err != nil {
		return nil, err
	}

	if repository.IncludeSnippets && !providerClient.SupportsSnippets() {
		slog.Warn("Provider does not support gists or snippets, so includeSnippets is ignored.",
			"provider", repository.Provider)
	}

	discoveredRepositories, err := providerClient.ListRepositories(ctx, forge.RepositoryJobOptions{
		BaseURL:         repository.BaseURL,
		IncludeStarred:  repository.IncludeStarred,
		IncludeSnippets: repository.IncludeSnippets,
	}, &forge.Credential{Username: credentialConfig.Username, APIKey: credentialConfig.APIKey})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Discovery failed, so we cannot know the full set of repositories —
		// signal an incomplete picture so local-mirror cleanup is skipped this
		// run.
		slog.Error("Provider repository discovery failed.",
			"provider", repository.Provider, "error", err.Error())
		return &runCounters{}, nil
	}

	slog.Info("Provider repository discovery completed.",
		"provider", repository.Provider, "repositories", len(discoveredRepositories))

	gitCredential := resolveGitCredential(credentialConfig)
	counters := &runCounters{complete: true}

	parallelErr := forEachParallel(ctx, repositoryConcurrency, discoveredRepositories,
		func(ctx context.Context, discovered forge.DiscoveredRepository) error {
			if strings.TrimSpace(discovered.CloneURL) == "" {
				return nil
			}

			if err := s.syncDiscoveredRepository(ctx, repository, discovered, credentialConfig, gitCredential,
				objectStorage, expectedMirrorDirectories, metadataConcurrency, downloadThrottle); err != nil {
				if ctx.Err() != nil {
					return err
				}

				var inaccessible *git.RemoteInaccessibleError
				if errors.As(err, &inaccessible) {
					// The remote is private, was removed (e.g. between
					// discovery and clone), or the credential no longer grants
					// access — a skippable per-repository condition, not a run
					// failure. Warn plainly and keep the git diagnostic at
					// debug.
					atomic.AddInt64(&counters.skippedInaccessible, 1)
					slog.Warn("Skipped repository because its remote is not accessible; it may be private, removed, or the credential lacks access.",
						"provider", repository.Provider, "repository", discovered.CloneURL)
					slog.Debug("Remote access failure detail.",
						"provider", repository.Provider, "repository", discovered.CloneURL, "detail", err.Error())
					return nil
				}

				slog.Error("Provider repository sync failed.",
					"provider", repository.Provider, "repository", discovered.CloneURL, "error", err.Error())
				return nil
			}

			atomic.AddInt64(&counters.synced, 1)
			return nil
		})
	if parallelErr != nil {
		return counters, parallelErr
	}

	counters.complete = ctx.Err() == nil
	return counters, nil
}

func (s *RepositorySyncService) syncDiscoveredRepository(
	ctx context.Context,
	repository *config.Repository,
	discovered forge.DiscoveredRepository,
	credentialConfig *config.Credential,
	gitCredential *git.Credential,
	objectStorage ObjectStorage,
	expectedMirrorDirectories map[string]struct{},
	metadataConcurrency int,
	downloadThrottle forge.Throttle,
) error {
	repositoryPrefix := resolveProviderPrefix(repository.Provider, discovered)

	s.expectedMirrorsMu.Lock()
	expectedMirrorDirectories[GetMirrorDirectoryName(repositoryPrefix)] = struct{}{}
	s.expectedMirrorsMu.Unlock()

	if err := s.syncRepositorySnapshot(ctx, config.ModeProvider, discovered.CloneURL, repositoryPrefix,
		repository.Cache, repository.LFS, gitCredential, objectStorage); err != nil {
		return err
	}

	if shouldBackUpProjectMetadata(repository, discovered) {
		return s.metadataSync.Sync(ctx, repository, discovered, repositoryPrefix, credentialConfig,
			objectStorage, metadataConcurrency, downloadThrottle)
	}
	return nil
}

// shouldBackUpProjectMetadata decides whether a repository's issues, merge
// requests, and releases are backed up: only for owned repositories — never
// for starred ones (even when includeStarred is set) and never for gists or
// snippets, which have no such data.
func shouldBackUpProjectMetadata(repository *config.Repository, discovered forge.DiscoveredRepository) bool {
	return (repository.IncludeIssues || repository.IncludeMergeRequests || repository.IncludeReleases) &&
		discovered.Kind == forge.KindRepository &&
		!discovered.IsStarred
}

// resolveProviderPrefix resolves the storage prefix for a discovered resource.
// Repositories use their clone URL's owner/repo hierarchy; gists and personal
// snippets have no such hierarchy and are keyed by id under the snippets/ root;
// project snippets nest under their owning project.
func resolveProviderPrefix(provider string, discovered forge.DiscoveredRepository) string {
	switch {
	case discovered.Kind == forge.KindGist:
		return paths.BuildSnippetResourcePrefix(provider, discovered.Identifier)

	case discovered.Kind == forge.KindSnippet && strings.TrimSpace(discovered.ParentURL) == "":
		return paths.BuildSnippetResourcePrefix(provider, discovered.Identifier)

	case discovered.Kind == forge.KindSnippet:
		projectInfo, err := paths.ParseRepositoryPath(discovered.ParentURL)
		if err != nil {
			// A hostile or malformed parent URL must not steer the key; fall
			// back to the standalone snippet layout.
			return paths.BuildSnippetResourcePrefix(provider, discovered.Identifier)
		}
		projectPrefix := paths.BuildProviderRepositoryPrefix(provider, projectInfo)
		return paths.BuildNestedSnippetPrefix(projectPrefix, discovered.Identifier)

	default:
		pathInfo, err := paths.ParseRepositoryPath(discovered.CloneURL)
		if err != nil {
			return paths.BuildSnippetResourcePrefix(provider, discovered.Identifier)
		}
		return paths.BuildProviderRepositoryPrefix(provider, pathInfo)
	}
}

func (s *RepositorySyncService) runURLMode(
	ctx context.Context,
	settings *config.Settings,
	repository *config.Repository,
	objectStorage ObjectStorage,
	expectedMirrorDirectories map[string]struct{},
	repositoryConcurrency int,
) (*runCounters, error) {
	if len(repository.Urls) == 0 {
		slog.Warn("Skipping URL repository job because url is missing.")
		return &runCounters{}, nil
	}

	// Collapse duplicate URLs to their first occurrence: the same repository
	// listed twice would otherwise be cloned and uploaded twice. Comparison is
	// case-insensitive because storage keys lowercase every segment, so
	// case-only variants map to the same destination. Warn on each one dropped
	// so a copy-paste mistake in the config is visible.
	repositoryUrls := make([]string, 0, len(repository.Urls))
	seenURLs := make(map[string]struct{}, len(repository.Urls))
	for _, configuredURL := range repository.Urls {
		if strings.TrimSpace(configuredURL) == "" {
			continue
		}
		if _, duplicate := seenURLs[strings.ToLower(strings.TrimSpace(configuredURL))]; duplicate {
			slog.Warn("Ignoring duplicate repository URL in URL job.", "repository", configuredURL)
			continue
		}
		seenURLs[strings.ToLower(strings.TrimSpace(configuredURL))] = struct{}{}
		repositoryUrls = append(repositoryUrls, configuredURL)
	}

	if len(repositoryUrls) == 0 {
		slog.Warn("Skipping URL repository job because it has no usable url.")
		return &runCounters{}, nil
	}

	var gitCredential *git.Credential
	if strings.TrimSpace(repository.Credential) != "" {
		credentialConfig, known := settings.Credentials[strings.ToLower(repository.Credential)]
		if !known {
			slog.Warn("Skipping URL repository job because credential is missing.",
				"credential", repository.Credential)
			return &runCounters{}, nil
		}
		gitCredential = resolveGitCredential(credentialConfig)
	}

	counters := &runCounters{complete: true}

	parallelErr := forEachParallel(ctx, repositoryConcurrency, repositoryUrls,
		func(ctx context.Context, url string) error {
			if err := s.syncURLRepository(ctx, url, repository, gitCredential, objectStorage, expectedMirrorDirectories); err != nil {
				if ctx.Err() != nil {
					return err
				}

				var inaccessible *git.RemoteInaccessibleError
				if errors.As(err, &inaccessible) {
					// The remote is private, was removed, or needs credentials
					// this job does not have — a skippable per-repository
					// condition, not a run failure.
					atomic.AddInt64(&counters.skippedInaccessible, 1)
					slog.Warn("Skipped repository because its remote is not accessible; it may be private, removed, or require credentials.",
						"repository", url)
					slog.Debug("Remote access failure detail.", "repository", url, "detail", err.Error())
					return nil
				}

				slog.Error("URL repository sync failed.", "repository", url, "error", err.Error())
				return nil
			}

			atomic.AddInt64(&counters.synced, 1)
			return nil
		})
	if parallelErr != nil {
		return counters, parallelErr
	}

	// The URL set is fully known from config (no discovery step), so the
	// mirror-cleanup picture stays complete even when an individual URL fails.
	counters.complete = true
	return counters, nil
}

func (s *RepositorySyncService) syncURLRepository(
	ctx context.Context,
	url string,
	repository *config.Repository,
	gitCredential *git.Credential,
	objectStorage ObjectStorage,
	expectedMirrorDirectories map[string]struct{},
) error {
	pathInfo, err := paths.ParseRepositoryPath(url)
	if err != nil {
		return err
	}
	repositoryPrefix := paths.BuildURLRepositoryPrefix(pathInfo)

	s.expectedMirrorsMu.Lock()
	expectedMirrorDirectories[GetMirrorDirectoryName(repositoryPrefix)] = struct{}{}
	s.expectedMirrorsMu.Unlock()

	return s.syncRepositorySnapshot(ctx, config.ModeURL, url, repositoryPrefix,
		repository.Cache, repository.LFS, gitCredential, objectStorage)
}

func (s *RepositorySyncService) syncRepositorySnapshot(
	ctx context.Context,
	mode, repositoryURL, repositoryPrefix string,
	cache, includeLFS bool,
	credential *git.Credential,
	objectStorage ObjectStorage,
) error {
	localPath := s.mirrorStore.GetMirrorPath(repositoryPrefix)

	slog.Info("Repository sync started.", "mode", mode, "repository", repositoryURL)
	slog.Debug("Repository working paths resolved.",
		"mode", mode, "repository", repositoryURL, "localPath", localPath, "targetPrefix", repositoryPrefix)

	if err := s.gitRepository.SyncBareRepository(ctx, repositoryURL, localPath, credential, cache, includeLFS); err != nil {
		return err
	}

	timestamp := time.Now().UTC()
	archiveObjectKey := paths.BuildArchiveObjectKey(repositoryPrefix, timestamp.Unix())

	if err := objectStorage.UploadDirectoryAsTarGz(ctx, localPath, archiveObjectKey); err != nil {
		return err
	}

	document, err := serializeMetadata(repositoryMetadataDocument{
		Mode:                 mode,
		RepositoryURL:        repositoryURL,
		UpdatedAtUnixSeconds: timestamp.Unix(),
	})
	if err != nil {
		return fmt.Errorf("serialize repository metadata: %w", err)
	}

	if err := objectStorage.UploadText(ctx, paths.BuildRepositoryMetadataObjectKey(repositoryPrefix), document); err != nil {
		return err
	}

	// A non-cached mirror exists only to build this snapshot; now that the
	// upload has succeeded, delete it so only one repository's worth of disk is
	// used at a time.
	if !cache {
		slog.Debug("Removing local mirror after upload (cache disabled).", "repository", repositoryURL)
		s.mirrorStore.TryDeleteMirror(repositoryPrefix)
	}

	slog.Info("Repository sync completed.",
		"mode", mode, "repository", repositoryURL, "destination", repositoryPrefix)
	return nil
}

// resolveGitCredential derives the HTTP basic-auth pair for a forge
// credential. Forges ignore the username and match the token, so a blank
// username falls back to "git".
func resolveGitCredential(credential *config.Credential) *git.Credential {
	if credential == nil || strings.TrimSpace(credential.APIKey) == "" {
		return nil
	}

	username := strings.TrimSpace(credential.Username)
	if username == "" {
		username = config.DefaultGitUsername
	}
	return &git.Credential{Username: username, Password: strings.TrimSpace(credential.APIKey)}
}

// forEachParallel runs fn over items with at most concurrency invocations in
// flight. A cancelled context stops the fan-out and surfaces the cancellation.
func forEachParallel[T any](ctx context.Context, concurrency int, items []T, fn func(context.Context, T) error) error {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency == 1 || len(items) <= 1 {
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}

	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			fail(err)
			break
		}
		if firstErr != nil {
			break
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			fail(ctx.Err())
		}

		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(ctx, item); err != nil {
				fail(err)
			}
		}(item)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
