using CLI.Configuration;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Paths;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class RetentionService
{
    private readonly Func<StorageConfig, IObjectStorageService> _objectStorageServiceFactory;

    public RetentionService(Func<StorageConfig, IObjectStorageService> objectStorageServiceFactory)
    {
        _objectStorageServiceFactory = objectStorageServiceFactory;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        var retentionDays = settings.Storage.Retention;
        if (retentionDays is null || retentionDays <= 0)
        {
            AppLogger.Info("Retention is disabled. Backups will be kept indefinitely.");
            return;
        }

        AppLogger.Info("Retention run started. retentionDays={RetentionDays}", retentionDays);

        var deletedCount = 0;
        var updatedIndexCount = 0;

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var cutoff = DateTimeOffset.UtcNow.AddDays(-retentionDays.Value);
        var backupRegistryObjectKey = StorageKeyBuilder.BuildBackupIndexRegistryObjectKey();

        var backupRegistryContent = await objectStorageService.GetTextIfExistsAsync(backupRegistryObjectKey, cancellationToken);
        var backupRegistry = ParseOrCreateBackupRegistry(backupRegistryContent);
        var knownIndexKeys = new HashSet<string>(backupRegistry.IndexKeys, StringComparer.Ordinal);
        var backupRegistryChanged = false;

        AppLogger.Info("Retention cutoff: {CutoffTimestamp}", AppLogger.FormatTimestamp(cutoff));

        foreach (var backupIndexObjectKey in knownIndexKeys.ToArray())
        {
            var backupIndexContent = await objectStorageService.GetTextIfExistsAsync(backupIndexObjectKey, cancellationToken);
            if (string.IsNullOrWhiteSpace(backupIndexContent))
            {
                backupRegistryChanged = true;
                knownIndexKeys.Remove(backupIndexObjectKey);
                AppLogger.Warn(
                    "Backup index is missing and will be removed from registry. indexObject={IndexObjectKey}",
                    backupIndexObjectKey);
                continue;
            }

            if (!StorageIndexDocuments.TryDeserialize<BackupRepositoryIndexDocument>(backupIndexContent, out var backupIndex) ||
                backupIndex is null)
            {
                AppLogger.Warn(
                    "Backup index contains invalid JSON and will be skipped. indexObject={IndexObjectKey}",
                    backupIndexObjectKey);
                continue;
            }

            backupIndex.Snapshots ??= [];

            var normalizedSnapshots = backupIndex.Snapshots
                .Where(IsValidSnapshot)
                .GroupBy(snapshot => snapshot.RootPrefix, StringComparer.Ordinal)
                .Select(group => group.OrderByDescending(snapshot => snapshot.TimestampUnixSeconds).First())
                .OrderByDescending(snapshot => snapshot.TimestampUnixSeconds)
                .ToList();

            if (normalizedSnapshots.Count == 0)
            {
                backupRegistryChanged = true;
                knownIndexKeys.Remove(backupIndexObjectKey);
                AppLogger.Warn(
                    "Backup index has no valid snapshots and will be removed from registry. indexObject={IndexObjectKey}",
                    backupIndexObjectKey);
                continue;
            }

            var newestSnapshotRootPrefix = normalizedSnapshots[0].RootPrefix;
            var expiredSnapshots = normalizedSnapshots
                .Skip(1)
                .Where(snapshot => DateTimeOffset.FromUnixTimeSeconds(snapshot.TimestampUnixSeconds) < cutoff)
                .ToArray();

            foreach (var snapshot in expiredSnapshots)
            {
                await DeleteSnapshotStorageAsync(objectStorageService, snapshot.RootPrefix, cancellationToken);
                deletedCount++;
                AppLogger.Info("Deleted expired backup snapshot. target={SnapshotTarget}", snapshot.RootPrefix);
            }

            var retainedSnapshots = normalizedSnapshots
                .Where(snapshot => string.Equals(snapshot.RootPrefix, newestSnapshotRootPrefix, StringComparison.Ordinal) ||
                                   DateTimeOffset.FromUnixTimeSeconds(snapshot.TimestampUnixSeconds) >= cutoff)
                .OrderByDescending(snapshot => snapshot.TimestampUnixSeconds)
                .ToList();

            if (!SnapshotsAreEqual(backupIndex.Snapshots, retainedSnapshots))
            {
                backupIndex.Snapshots = retainedSnapshots;
                await objectStorageService.UploadTextAsync(
                    backupIndexObjectKey,
                    StorageIndexDocuments.Serialize(backupIndex),
                    cancellationToken);
                updatedIndexCount++;
            }
        }

        if (backupRegistryChanged)
        {
            backupRegistry.IndexKeys = knownIndexKeys.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                backupRegistryObjectKey,
                StorageIndexDocuments.Serialize(backupRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            "Retention run completed. deletedSnapshots={DeletedSnapshots}, updatedIndexes={UpdatedIndexes}.",
            deletedCount,
            updatedIndexCount);
    }

    private static bool IsValidSnapshot(BackupSnapshotDocument snapshot)
    {
        return !string.IsNullOrWhiteSpace(snapshot.RootPrefix) && snapshot.TimestampUnixSeconds > 0;
    }

    private static async Task DeleteSnapshotStorageAsync(
        IObjectStorageService objectStorageService,
        string snapshotRootPrefix,
        CancellationToken cancellationToken)
    {
        if (snapshotRootPrefix.EndsWith(".tar.gz", StringComparison.Ordinal))
        {
            await objectStorageService.DeleteObjectsAsync([snapshotRootPrefix], cancellationToken);
            return;
        }

        // TODO(2026-06-30): Remove legacy prefix deletion fallback after pre-launch data migration is complete.
        AppLogger.Debug(
            "Using legacy retention deletion for snapshot prefix. prefix={SnapshotPrefix}",
            snapshotRootPrefix);
        await objectStorageService.DeletePrefixAsync(snapshotRootPrefix, cancellationToken);
    }

    private static bool SnapshotsAreEqual(
        IReadOnlyList<BackupSnapshotDocument> left,
        IReadOnlyList<BackupSnapshotDocument> right)
    {
        if (left.Count != right.Count)
        {
            return false;
        }

        for (var i = 0; i < left.Count; i++)
        {
            var leftSnapshot = left[i];
            var rightSnapshot = right[i];

            if (!string.Equals(leftSnapshot.RootPrefix, rightSnapshot.RootPrefix, StringComparison.Ordinal) ||
                leftSnapshot.TimestampUnixSeconds != rightSnapshot.TimestampUnixSeconds ||
                !string.Equals(leftSnapshot.ArchiveSha256, rightSnapshot.ArchiveSha256, StringComparison.Ordinal))
            {
                return false;
            }
        }

        return true;
    }

    private static BackupIndexRegistryDocument ParseOrCreateBackupRegistry(string? json)
    {
        if (string.IsNullOrWhiteSpace(json))
        {
            return new BackupIndexRegistryDocument();
        }

        if (!StorageIndexDocuments.TryDeserialize<BackupIndexRegistryDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn("Backup index registry is invalid JSON. Rebuilding from discovered state.");
            return new BackupIndexRegistryDocument();
        }

        parsed.IndexKeys = (parsed.IndexKeys ?? [])
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .Select(value => value.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToList();
        return parsed;
    }
}
