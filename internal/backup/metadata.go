package backup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/forge"
	"github.com/neurekadev/git-backup/internal/paths"
	"github.com/neurekadev/git-backup/internal/store"
)

// MetadataSyncService backs up a single owned repository's issues, merge
// requests, and releases (with embedded comments where applicable and, when
// enabled, downloaded attachments/assets) as latest-state JSON documents under
// the repository's storage prefix, then reconciles the stored set to match
// what the provider returned.
type MetadataSyncService struct {
	providerFactory *forge.Factory
}

// NewMetadataSyncService wires the metadata sync service.
func NewMetadataSyncService(providerFactory *forge.Factory) *MetadataSyncService {
	return &MetadataSyncService{providerFactory: providerFactory}
}

// collectionSpec is the per-collection descriptor (labels, keys, and the
// map/serialize plumbing) shared by the three collections.
type collectionSpec[T any] struct {
	label, supportLabel, countLabel string
	supported                       bool
	list                            func(ctx context.Context, deliver func(T) error) error
	collectionPrefix                string
	manifestObjectKey               string
	getSlug                         func(T) string
	buildObjectKey                  func(slug string) string
	buildAttachmentObjectKey        func(slug, fileName string) string
	toManifestEntry                 func(T) any
	includeArtifacts                bool
}

// collectionBackup carries the ambient dependencies shared by all three
// collections of one repository's run.
type collectionBackup struct {
	repository        *config.Repository
	metadataContext   *forge.MetadataContext
	metadataClient    forge.ProjectMetadataProviderClient
	credential        *forge.Credential
	repositoryDisplay string
	objectStorage     ObjectStorage
}

// Sync backs up the requested metadata collections of one repository.
func (s *MetadataSyncService) Sync(
	ctx context.Context,
	repository *config.Repository,
	discovered forge.DiscoveredRepository,
	repositoryPrefix string,
	credential *config.Credential,
	objectStorage ObjectStorage,
	concurrency int,
	downloadThrottle forge.Throttle,
) error {
	metadataClient := s.providerFactory.TryResolveMetadata(repository.Provider)
	if metadataClient == nil {
		slog.Debug("Provider does not support project metadata backup.", "provider", repository.Provider)
		return nil
	}

	repositoryDisplay := discovered.WebURL
	if repositoryDisplay == "" {
		repositoryDisplay = discovered.CloneURL
	}
	metadataContext := &forge.MetadataContext{
		CloneURL:          discovered.CloneURL,
		WebURL:            discovered.WebURL,
		ProviderProjectID: discovered.ProviderProjectID,
		BaseURL:           repository.BaseURL,
		Concurrency:       max(concurrency, 1),
		DownloadThrottle:  downloadThrottle,
	}
	forgeCredential := &forge.Credential{Username: credential.Username, APIKey: credential.APIKey}

	backup := &collectionBackup{
		repository:        repository,
		metadataContext:   metadataContext,
		metadataClient:    metadataClient,
		credential:        forgeCredential,
		repositoryDisplay: repositoryDisplay,
		objectStorage:     objectStorage,
	}

	if repository.IncludeIssues {
		s.backUpCollection(ctx, collectionSpec[*forge.Issue]{
			label:        "Issue",
			supportLabel: "issues",
			countLabel:   "issues",
			supported:    metadataClient.SupportsIssues(),
			list: func(ctx context.Context, deliver func(*forge.Issue) error) error {
				return metadataClient.ListIssues(ctx, metadataContext, forgeCredential, func(issue forge.Issue) error {
					return deliver(&issue)
				})
			},
			collectionPrefix:  paths.BuildIssuesCollectionPrefix(repositoryPrefix),
			manifestObjectKey: paths.BuildIssuesManifestObjectKey(repositoryPrefix),
			getSlug:           func(issue *forge.Issue) string { return fmt.Sprintf("%d", issue.Number) },
			buildObjectKey:    func(slug string) string { return paths.BuildIssueObjectKey(repositoryPrefix, slug) },
			buildAttachmentObjectKey: func(slug, fileName string) string {
				return paths.BuildIssueAttachmentObjectKey(repositoryPrefix, slug, fileName)
			},
			toManifestEntry: func(issue *forge.Issue) any {
				return collectionManifestEntry{Number: issue.Number, Title: &issue.Title, State: issue.State, UpdatedAt: issue.UpdatedAt}
			},
			includeArtifacts: repository.IncludeIssueArtifacts,
		}, backup)
	}

	if repository.IncludeMergeRequests {
		s.backUpCollection(ctx, collectionSpec[*forge.MergeRequest]{
			label:        "Merge request",
			supportLabel: "merge requests",
			countLabel:   "mergeRequests",
			supported:    metadataClient.SupportsMergeRequests(),
			list: func(ctx context.Context, deliver func(*forge.MergeRequest) error) error {
				return metadataClient.ListMergeRequests(ctx, metadataContext, forgeCredential, func(mergeRequest forge.MergeRequest) error {
					return deliver(&mergeRequest)
				})
			},
			collectionPrefix:  paths.BuildMergeRequestsCollectionPrefix(repositoryPrefix),
			manifestObjectKey: paths.BuildMergeRequestsManifestObjectKey(repositoryPrefix),
			getSlug:           func(mergeRequest *forge.MergeRequest) string { return fmt.Sprintf("%d", mergeRequest.Number) },
			buildObjectKey:    func(slug string) string { return paths.BuildMergeRequestObjectKey(repositoryPrefix, slug) },
			buildAttachmentObjectKey: func(slug, fileName string) string {
				return paths.BuildMergeRequestAttachmentObjectKey(repositoryPrefix, slug, fileName)
			},
			toManifestEntry: func(mergeRequest *forge.MergeRequest) any {
				return collectionManifestEntry{Number: mergeRequest.Number, Title: &mergeRequest.Title, State: mergeRequest.State, UpdatedAt: mergeRequest.UpdatedAt}
			},
			includeArtifacts: repository.IncludeMergeRequestsArtifacts,
		}, backup)
	}

	if repository.IncludeReleases {
		s.backUpCollection(ctx, collectionSpec[*forge.Release]{
			label:        "Release",
			supportLabel: "releases",
			countLabel:   "releases",
			supported:    metadataClient.SupportsReleases(),
			list: func(ctx context.Context, deliver func(*forge.Release) error) error {
				return metadataClient.ListReleases(ctx, metadataContext, forgeCredential, func(release forge.Release) error {
					return deliver(&release)
				})
			},
			collectionPrefix:  paths.BuildReleasesCollectionPrefix(repositoryPrefix),
			manifestObjectKey: paths.BuildReleasesManifestObjectKey(repositoryPrefix),
			getSlug:           func(release *forge.Release) string { return resolveReleaseSlug(release.Tag) },
			buildObjectKey:    func(slug string) string { return paths.BuildReleaseObjectKey(repositoryPrefix, slug) },
			buildAttachmentObjectKey: func(slug, fileName string) string {
				return paths.BuildReleaseAttachmentObjectKey(repositoryPrefix, slug, fileName)
			},
			toManifestEntry: func(release *forge.Release) any {
				return releaseManifestEntry{Tag: release.Tag, Name: release.Name, PublishedAt: release.PublishedAt}
			},
			includeArtifacts: repository.IncludeReleaseArtifacts,
		}, backup)
	}

	return nil
}

// backUpCollection backs up one metadata collection (issues, merge requests,
// or releases). A partial fetch never drives a reconciliation delete: a
// listing failure leaves the previous manifest untouched.
func (s *MetadataSyncService) backUpCollection[T any](ctx context.Context, spec collectionSpec[T], backup *collectionBackup) {
	if !spec.supported {
		slog.Debug("Provider does not support collection.", "collection", spec.supportLabel, "provider", backup.repository.Provider)
		return
	}

	slog.Info("Collection backup started.", "collection", spec.label, "repository", backup.repositoryDisplay)

	backedUp, completed := s.syncCollection(ctx, spec, backup)

	if completed {
		slog.Info("Collection backup completed.",
			"collection", spec.label, "repository", backup.repositoryDisplay, "count", backedUp)
	} else {
		// The listing failed partway, so the set is incomplete: say so rather
		// than reporting a completed backup, which would read as though
		// everything had been captured.
		slog.Warn("Collection backup finished incomplete; stored items were kept and nothing was deleted.",
			"collection", spec.label, "repository", backup.repositoryDisplay, "count", backedUp)
	}
}

func (s *MetadataSyncService) syncCollection[T any](ctx context.Context, spec collectionSpec[T], backup *collectionBackup) (int, bool) {
	downloadArtifacts := spec.includeArtifacts && backup.metadataClient.SupportsArtifacts()
	var anyItemFailed atomic.Bool
	backedUp := 0
	manifestEntries := make([]any, 0, 16)
	fetchedSlugs := make(map[string]struct{})
	var accumulateLock sync.Mutex
	listingFailed := false

	// Stream the collection so it is never held in memory all at once.
	// Per-item work (attachment download + document upload) overlaps up to the
	// configured degree with backpressure from the source, so peak memory is
	// one provider page plus the in-flight items rather than every issue/MR
	// with its full comment thread. A single item's failure is logged and
	// flagged (not thrown) so it never discards the rest of the run.
	items := make(chan T, max(backup.metadataContext.Concurrency, 1))
	producerDone := make(chan error, 1)
	go func() {
		producerDone <- spec.list(ctx, func(item T) error {
			select {
			case items <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	var workers sync.WaitGroup
	for worker := 0; worker < max(backup.metadataContext.Concurrency, 1); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range items {
				slug := spec.getSlug(item)

				if err := s.processItem(ctx, item, slug, spec, backup, downloadArtifacts, &accumulateLock, &manifestEntries, fetchedSlugs, &backedUp); err != nil {
					if ctx.Err() != nil {
						return
					}
					anyItemFailed.Store(true)
					slog.Error("Collection item backup failed.",
						"collection", spec.label, "repository", backup.repositoryDisplay, "slug", slug, "error", err.Error())
				}
			}
		}()
	}

	produceErr := <-producerDone
	close(items)
	workers.Wait()

	if produceErr != nil {
		if ctx.Err() != nil {
			return backedUp, false
		}
		// Listing/paging (or a per-item comment fetch) failed partway through
		// the stream, so the set is incomplete. Leave the previous manifest in
		// place and delete nothing this run.
		listingFailed = true
		slog.Error("Collection backup failed while listing.",
			"collection", spec.label, "repository", backup.repositoryDisplay, "error", produceErr.Error())
	}

	if listingFailed {
		return backedUp, false
	}

	manifest, err := serializeMetadata(manifestEntries)
	if err != nil {
		slog.Error("Collection manifest serialization failed.",
			"collection", spec.label, "repository", backup.repositoryDisplay, "error", err.Error())
		return backedUp, false
	}
	if err := backup.objectStorage.UploadText(ctx, spec.manifestObjectKey, manifest); err != nil {
		return backedUp, false
	}

	// Reconciliation deletes stored documents the provider no longer returns.
	// Skip it when any item failed this run so an item whose document did not
	// upload is never mistaken for one removed upstream and deleted — a
	// partial set must not drive deletes.
	if anyItemFailed.Load() {
		slog.Warn("Collection reconciliation skipped because one or more items failed to back up.",
			"collection", spec.label, "repository", backup.repositoryDisplay)
		return backedUp, true
	}

	s.reconcile(ctx, fetchedSlugs, spec.collectionPrefix, spec.manifestObjectKey, backup.objectStorage)
	return backedUp, true
}

// processItem uploads one item's document (after downloading its attachments
// when enabled) and records its manifest entry. The returned error is the
// item's failure signal; it never aborts the run.
func (s *MetadataSyncService) processItem[T any](
	ctx context.Context,
	item T,
	slug string,
	spec collectionSpec[T],
	backup *collectionBackup,
	downloadArtifacts bool,
	accumulateLock *sync.Mutex,
	manifestEntries *[]any,
	fetchedSlugs map[string]struct{},
	backedUp *int,
) error {
	if downloadArtifacts {
		if err := s.downloadAttachments(ctx, item, slug, spec.buildAttachmentObjectKey, backup); err != nil {
			return err
		}
	} else {
		// Artifacts are gated off, so record no attachment references at all.
		clearAttachments(item)
	}

	normalizeDocument(item)

	document, err := serializeMetadata(item)
	if err != nil {
		return fmt.Errorf("serialize document: %w", err)
	}

	if err := backup.objectStorage.UploadText(ctx, spec.buildObjectKey(slug), document); err != nil {
		return err
	}

	accumulateLock.Lock()
	*manifestEntries = append(*manifestEntries, spec.toManifestEntry(item))
	// Record the slug even on failure paths above so a reconcile can never
	// treat this still-present item as removed (reconcile is skipped this run
	// regardless when an item failed).
	fetchedSlugs[slug] = struct{}{}
	*backedUp++
	accumulateLock.Unlock()
	return nil
}

// downloadAttachments streams each of the item's attachments into storage.
// Reference-only assets (e.g. a GitLab release link outside the instance) are
// recorded but never fetched, so the credential is never sent to a third-party
// host. One bad attachment keeps its reference (with no storage key) so the
// document still records that the file existed; it must not fail the item.
func (s *MetadataSyncService) downloadAttachments[T any](
	ctx context.Context,
	item T,
	slug string,
	buildAttachmentObjectKey func(slug, fileName string) string,
	backup *collectionBackup,
) error {
	attachments := attachmentsOf(item)
	for i := range attachments {
		attachment := &attachments[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		if !attachment.Downloadable {
			continue
		}

		// Bound how many attachments are downloaded at once across the whole
		// run. A cancellation while waiting propagates before the acquire
		// succeeds, so no release runs.
		if err := backup.metadataContext.DownloadThrottle.Acquire(ctx); err != nil {
			return err
		}

		func() {
			defer backup.metadataContext.DownloadThrottle.Release()

			stream, err := backup.metadataClient.OpenAttachment(ctx, backup.metadataContext, backup.credential, attachment.DownloadURL)
			if err != nil {
				slog.Error("Attachment backup failed.",
					"originalPath", forge.RedactURL(attachment.OriginalPath), "error", err.Error())
				return
			}
			defer func() { _ = stream.Close() }()

			objectKey := buildAttachmentObjectKey(slug, attachment.FileName)
			contentType := store.ResolveContentType(attachment.FileName)

			knownLength := stream.KnownLength
			if err := backup.objectStorage.UploadStream(ctx, objectKey, stream, contentType, knownLength); err != nil {
				slog.Error("Attachment backup failed.",
					"originalPath", forge.RedactURL(attachment.OriginalPath), "error", err.Error())
				return
			}

			attachment.StorageKey = &objectKey
			attachment.ContentType = &contentType
			// Prefer the length the server declared; fall back to the
			// provider-reported size (set for release assets). The stream
			// itself cannot answer — it is not seekable.
			if knownLength >= 0 {
				attachment.SizeBytes = &knownLength
			}
		}()
	}
	return nil
}

// reconcile deletes stored documents (and attachment subtrees) for items the
// provider no longer returns, so the backup mirrors upstream. Runs only after
// a complete, successful fetch — the manifest just written is left in place.
func (s *MetadataSyncService) reconcile(
	ctx context.Context,
	fetchedSlugs map[string]struct{},
	collectionPrefix, manifestObjectKey string,
	objectStorage ObjectStorage,
) {
	normalizedPrefix := paths.EnsurePrefix(collectionPrefix)
	existingKeys, err := objectStorage.ListObjectKeys(ctx, collectionPrefix)
	if err != nil {
		return
	}

	var keysToDelete []string
	for _, objectKey := range existingKeys {
		if objectKey == manifestObjectKey || !strings.HasPrefix(objectKey, normalizedPrefix) {
			continue
		}

		relativeKey := objectKey[len(normalizedPrefix):]
		if slug, ok := reconcilableSlug(relativeKey); ok {
			if _, fetched := fetchedSlugs[slug]; !fetched {
				keysToDelete = append(keysToDelete, objectKey)
			}
		}
	}

	if len(keysToDelete) > 0 {
		slog.Debug("Reconciling stored collection.", "removedObjects", len(keysToDelete))
		if err := objectStorage.DeleteObjects(ctx, keysToDelete); err != nil {
			return
		}
	}
}

// reconcilableSlug extracts the owning item slug from a collection-relative
// key: {slug}.json or attachments/{slug}/... It returns false for the manifest
// and anything else, so those are never reconciled away.
func reconcilableSlug(relativeKey string) (string, bool) {
	firstSlash := strings.Index(relativeKey, "/")
	if firstSlash < 0 {
		if !strings.HasSuffix(relativeKey, ".json") {
			return "", false
		}
		slug := strings.TrimSuffix(relativeKey, ".json")
		return slug, slug != ""
	}

	if relativeKey[:firstSlash] != paths.AttachmentsCollectionSegment {
		return "", false
	}

	afterAttachments := relativeKey[firstSlash+1:]
	nextSlash := strings.Index(afterAttachments, "/")
	if nextSlash < 0 {
		return afterAttachments, afterAttachments != ""
	}
	slug := afterAttachments[:nextSlash]
	return slug, slug != ""
}

// resolveReleaseSlug turns a release tag into a safe storage-key leaf. It
// guards the reserved manifest base name so a tag literally named "index"
// cannot collide with the collection's index.json.
func resolveReleaseSlug(tag string) string {
	slug := forge.SanitizeFileName(tag)
	if slug == "index" {
		slug = slug + "-" + forge.ShortHash(tag)
	}
	return slug
}

// attachmentsOf returns the item's attachment slice. The slice header shares
// the backing array, so index writes mutate the item's own elements.
func attachmentsOf(item any) []forge.Attachment {
	switch typed := item.(type) {
	case *forge.Issue:
		return typed.Attachments
	case *forge.MergeRequest:
		return typed.Attachments
	case *forge.Release:
		return typed.Attachments
	default:
		return nil
	}
}

// clearAttachments drops every attachment reference, used when artifacts are
// gated off. The field serializes as an empty array, never null.
func clearAttachments(item any) {
	switch typed := item.(type) {
	case *forge.Issue:
		typed.Attachments = []forge.Attachment{}
	case *forge.MergeRequest:
		typed.Attachments = []forge.Attachment{}
	case *forge.Release:
		typed.Attachments = []forge.Attachment{}
	}
}

// normalizeDocument guarantees the stored JSON matches the documented format:
// comment threads and attachment lists are always arrays (never null), and the
// embedded attachment slices are re-shared after any mutation.
func normalizeDocument(item any) {
	switch typed := item.(type) {
	case *forge.Issue:
		if typed.Comments == nil {
			typed.Comments = []forge.Comment{}
		}
		if typed.Attachments == nil {
			typed.Attachments = []forge.Attachment{}
		}
	case *forge.MergeRequest:
		if typed.Comments == nil {
			typed.Comments = []forge.Comment{}
		}
		if typed.Attachments == nil {
			typed.Attachments = []forge.Attachment{}
		}
	case *forge.Release:
		if typed.Attachments == nil {
			typed.Attachments = []forge.Attachment{}
		}
	}
}
