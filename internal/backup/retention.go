package backup

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/paths"
	"github.com/neurekadev/git-backup/internal/schedule"
)

// snapshot is one archive object with the timestamp encoded in its key.
type snapshot struct {
	objectKey string
	timestamp int64
}

// RetentionService prunes expired snapshots. The object listing is the source
// of truth: every archive object encodes its own timestamp in its key, so
// retention needs no side index.
type RetentionService struct {
	storageFactory StorageFactory

	retentionMinimumZeroWarningShown bool
	warningMu                        sync.Mutex
}

// NewRetentionService wires the retention service.
func NewRetentionService(storageFactory StorageFactory) *RetentionService {
	return &RetentionService{storageFactory: storageFactory}
}

// Run prunes expired snapshots once. Disabled when retention is unset or not
// positive; the newest retentionMinimum snapshots of every repository are
// always protected.
func (s *RetentionService) Run(ctx context.Context, settings *config.Settings) error {
	retentionDays := settings.Storage.Retention
	retentionMinimum := max(settings.Storage.RetentionMinimum, 0)
	if retentionDays <= 0 {
		slog.Info("Retention is disabled. Repository snapshots will be kept indefinitely.")
		return nil
	}

	if retentionMinimum == 0 {
		s.warningMu.Lock()
		shown := s.retentionMinimumZeroWarningShown
		s.retentionMinimumZeroWarningShown = true
		s.warningMu.Unlock()
		if !shown {
			slog.Warn("Retention minimum is set to 0. Repository snapshots can be deleted after the retention window, including repositories removed from configuration or whose URL changed.")
		}
	} else {
		s.warningMu.Lock()
		s.retentionMinimumZeroWarningShown = false
		s.warningMu.Unlock()
	}

	slog.Info("Retention run started.", "retentionDays", retentionDays, "retentionMinimum", retentionMinimum)

	objectStorage, err := s.storageFactory(settings)
	if err != nil {
		return err
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	slog.Info("Retention cutoff resolved.", "cutoff", schedule.FormatTimestamp(cutoff))

	deletedSnapshots, deletedOrphans, emptied, err := s.apply(ctx, objectStorage, cutoff, retentionMinimum)
	if err != nil {
		return err
	}

	slog.Info("Retention run completed.",
		"deletedSnapshots", deletedSnapshots,
		"deletedOrphanObjects", deletedOrphans,
		"emptiedRepositories", emptied)
	return nil
}

// apply prunes expired snapshots grouped by repository prefix. Snapshots are
// ordered newest-first, the newest retentionMinimum are always protected, and
// anything older than cutoff beyond that is deleted in a single batched call.
// When a repository loses all of its snapshots, its remaining non-archive
// objects (the advisory metadata.json and any issues/, merge-requests/, and
// releases/ documents and attachments) are removed too so no orphaned prefix
// is left behind.
func (s *RetentionService) apply(ctx context.Context, objectStorage ObjectStorage, cutoff time.Time, retentionMinimum int) (int, int, int, error) {
	// Snapshots live under two roots: repositories/ (repos and owned project
	// snippets) and snippets/ (gists and personal snippets). Retention treats
	// both the same way. The two roots are independent subtrees, so list them
	// concurrently instead of one full paginated walk after the other.
	type listing struct {
		keys []string
		err  error
	}
	listings := make([]listing, 2)
	var wg sync.WaitGroup
	for i, root := range []string{paths.RepositoriesPrefix, paths.SnippetsPrefix} {
		wg.Add(1)
		go func(i int, root string) {
			defer wg.Done()
			keys, err := objectStorage.ListObjectKeys(ctx, root)
			listings[i] = listing{keys: keys, err: err}
		}(i, root)
	}
	wg.Wait()

	for _, result := range listings {
		if result.err != nil {
			return 0, 0, 0, result.err
		}
	}

	// Classify every key exactly once here: archive keys feed the retention
	// grouping below, and everything else is a candidate orphan collected in
	// nonArchiveKeys — so the orphan pass can filter that list rather than
	// re-parsing every key a second time. The two prefix listings are
	// classified in place rather than concatenated into a combined list first,
	// which would hold a redundant third copy of the whole key set.
	snapshotsByRepository := make(map[string][]snapshot)
	var nonArchiveKeys []string
	classify := func(objectKeys []string) {
		for _, objectKey := range objectKeys {
			timestamp, ok := paths.TryGetArchiveTimestamp(objectKey)
			if !ok {
				nonArchiveKeys = append(nonArchiveKeys, objectKey)
				continue
			}
			repositoryPrefix := paths.GetParentPrefix(objectKey)
			snapshotsByRepository[repositoryPrefix] = append(snapshotsByRepository[repositoryPrefix], snapshot{
				objectKey: objectKey,
				timestamp: timestamp,
			})
		}
	}
	classify(listings[0].keys)
	classify(listings[1].keys)

	var expiredKeys []string
	emptiedRepositories := make(map[string]struct{})

	for repositoryPrefix, snapshots := range snapshotsByRepository {
		ordered := make([]snapshot, len(snapshots))
		copy(ordered, snapshots)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].timestamp > ordered[j].timestamp })

		protectedCount := min(retentionMinimum, len(ordered))
		var expired []snapshot
		for _, item := range ordered[protectedCount:] {
			if time.Unix(item.timestamp, 0).UTC().Before(cutoff) {
				expired = append(expired, item)
			}
		}

		for _, item := range expired {
			expiredKeys = append(expiredKeys, item.objectKey)
		}
		if len(expired) == len(ordered) {
			emptiedRepositories[repositoryPrefix] = struct{}{}
		}
	}

	// Non-archive objects belonging to an emptied repository: the advisory
	// metadata.json plus the issues/, merge-requests/, and releases/ subtrees
	// (documents and attachments). Project snippets nest under
	// {prefix}/snippets/{id} and are independent repositories with their own
	// snapshots and retention, so they are intentionally left untouched here.
	//
	// Precompute the deep collection prefixes for each emptied repository once
	// so each key probes a set instead of rebuilding and comparing strings.
	reclaimableCollectionPrefixes := make(map[string]struct{}, len(emptiedRepositories)*3)
	for repositoryPrefix := range emptiedRepositories {
		for _, segment := range []string{paths.IssuesCollectionSegment, paths.MergeRequestsCollectionSegment, paths.ReleasesCollectionSegment} {
			reclaimableCollectionPrefixes[repositoryPrefix+"/"+segment] = struct{}{}
		}
	}

	var orphanKeys []string
	for _, objectKey := range nonArchiveKeys {
		if isReclaimableOrphan(objectKey, emptiedRepositories, reclaimableCollectionPrefixes) {
			orphanKeys = append(orphanKeys, objectKey)
		}
	}

	keysToDelete := append(append([]string{}, expiredKeys...), orphanKeys...)
	if len(keysToDelete) > 0 {
		if err := objectStorage.DeleteObjects(ctx, keysToDelete); err != nil {
			return 0, 0, 0, err
		}
	}

	return len(expiredKeys), len(orphanKeys), len(emptiedRepositories), nil
}

// isReclaimableOrphan reports whether a non-archive object belongs to an
// emptied repository. The advisory metadata.json is a direct child of the
// repository prefix; issue/merge-request/release documents and their
// attachments nest deeper. This key's own ancestor prefixes are walked —
// bounded by its segment count — and probed against the reclaimed set, rather
// than testing every emptied repository's prefixes against every key: both of
// those grow with the bucket, so scanning one against the other degrades when
// a batch of repositories expires together; this does not.
func isReclaimableOrphan(objectKey string, emptiedRepositories, reclaimableCollectionPrefixes map[string]struct{}) bool {
	if _, emptied := emptiedRepositories[paths.GetParentPrefix(objectKey)]; emptied {
		return true
	}

	for i := strings.Index(objectKey, "/"); i > 0; {
		if _, reclaimable := reclaimableCollectionPrefixes[objectKey[:i]]; reclaimable {
			return true
		}
		next := strings.Index(objectKey[i+1:], "/")
		if next < 0 {
			break
		}
		i = next + i + 1
	}
	return false
}
