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
            AppLogger.Info("Retention is disabled. Backups and mirrors will be kept indefinitely.");
            return;
        }

        AppLogger.Info("Retention run started. retentionDays={RetentionDays}", retentionDays);

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var cutoff = DateTimeOffset.UtcNow.AddDays(-retentionDays.Value);
        AppLogger.Info("Retention cutoff: {CutoffTimestamp}", AppLogger.FormatTimestamp(cutoff));

        var backupRetention = await ApplyBackupRetentionAsync(objectStorageService, cutoff, cancellationToken);
        var mirrorRetention = await ApplyMirrorRetentionAsync(objectStorageService, cutoff, cancellationToken);

        AppLogger.Info(
            "Retention run completed. deletedSnapshots={DeletedSnapshots}, updatedIndexes={UpdatedIndexes}.",
            backupRetention.DeletedSnapshots + mirrorRetention.DeletedSnapshots,
            backupRetention.UpdatedIndexes + mirrorRetention.UpdatedIndexes);
    }

    private async Task<(int DeletedSnapshots, int UpdatedIndexes)> ApplyBackupRetentionAsync(
        IObjectStorageService objectStorageService,
        DateTimeOffset cutoff,
        CancellationToken cancellationToken)
    {
        var deletedCount = 0;
        var updatedIndexCount = 0;

        var backupRegistryObjectKey = StorageKeyBuilder.BuildBackupIndexRegistryObjectKey();
        var backupRegistryContent = await objectStorageService.GetTextIfExistsAsync(backupRegistryObjectKey, cancellationToken);
        var backupRegistry = ParseOrCreateBackupRegistry(backupRegistryContent);
        var knownIndexKeys = new HashSet<string>(backupRegistry.IndexKeys, StringComparer.Ordinal);
        var backupRegistryChanged = false;

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
                .Where(IsValidBackupSnapshot)
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

            if (!BackupSnapshotsAreEqual(backupIndex.Snapshots, retainedSnapshots))
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
            "Backup retention cleanup completed. deletedSnapshots={DeletedSnapshots}, updatedIndexes={UpdatedIndexes}.",
            deletedCount,
            updatedIndexCount);
        return (deletedCount, updatedIndexCount);
    }

    private async Task<(int DeletedSnapshots, int UpdatedIndexes)> ApplyMirrorRetentionAsync(
        IObjectStorageService objectStorageService,
        DateTimeOffset cutoff,
        CancellationToken cancellationToken)
    {
        var deletedCount = 0;
        var updatedIndexCount = 0;

        var mirrorRegistryObjectKey = StorageKeyBuilder.BuildMirrorRegistryObjectKey();
        var mirrorRegistryContent = await objectStorageService.GetTextIfExistsAsync(mirrorRegistryObjectKey, cancellationToken);
        var mirrorRegistry = ParseOrCreateMirrorRegistry(mirrorRegistryContent);
        var knownIndexKeys = new HashSet<string>(mirrorRegistry.IndexKeys, StringComparer.Ordinal);
        var mirrorRegistryChanged = false;

        foreach (var mirrorIndexObjectKey in knownIndexKeys.ToArray())
        {
            var mirrorIndexContent = await objectStorageService.GetTextIfExistsAsync(mirrorIndexObjectKey, cancellationToken);
            if (string.IsNullOrWhiteSpace(mirrorIndexContent))
            {
                mirrorRegistryChanged = true;
                knownIndexKeys.Remove(mirrorIndexObjectKey);
                AppLogger.Warn(
                    "Mirror index is missing and will be removed from registry. indexObject={IndexObjectKey}",
                    mirrorIndexObjectKey);
                continue;
            }

            if (!StorageIndexDocuments.TryDeserialize<MirrorRepositoryIndexDocument>(mirrorIndexContent, out var mirrorIndex) ||
                mirrorIndex is null)
            {
                AppLogger.Warn(
                    "Mirror index contains invalid JSON and will be skipped. indexObject={IndexObjectKey}",
                    mirrorIndexObjectKey);
                continue;
            }

            mirrorIndex.Snapshots ??= [];

            var normalizedSnapshots = mirrorIndex.Snapshots
                .Where(IsValidMirrorSnapshot)
                .GroupBy(snapshot => snapshot.RootPrefix, StringComparer.Ordinal)
                .Select(group => group.OrderByDescending(snapshot => snapshot.TimestampUnixSeconds).First())
                .OrderByDescending(snapshot => snapshot.TimestampUnixSeconds)
                .ToList();

            if (normalizedSnapshots.Count == 0)
            {
                mirrorRegistryChanged = true;
                knownIndexKeys.Remove(mirrorIndexObjectKey);
                AppLogger.Warn(
                    "Mirror index has no valid snapshots and will be removed from registry. indexObject={IndexObjectKey}",
                    mirrorIndexObjectKey);
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
                AppLogger.Info("Deleted expired mirror snapshot. target={SnapshotTarget}", snapshot.RootPrefix);
            }

            var retainedSnapshots = normalizedSnapshots
                .Where(snapshot => string.Equals(snapshot.RootPrefix, newestSnapshotRootPrefix, StringComparison.Ordinal) ||
                                   DateTimeOffset.FromUnixTimeSeconds(snapshot.TimestampUnixSeconds) >= cutoff)
                .OrderByDescending(snapshot => snapshot.TimestampUnixSeconds)
                .ToList();

            if (!MirrorSnapshotsAreEqual(mirrorIndex.Snapshots, retainedSnapshots))
            {
                mirrorIndex.Snapshots = retainedSnapshots;
                await objectStorageService.UploadTextAsync(
                    mirrorIndexObjectKey,
                    StorageIndexDocuments.Serialize(mirrorIndex),
                    cancellationToken);
                updatedIndexCount++;
            }
        }

        if (mirrorRegistryChanged)
        {
            mirrorRegistry.IndexKeys = knownIndexKeys.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                mirrorRegistryObjectKey,
                StorageIndexDocuments.Serialize(mirrorRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            "Mirror retention cleanup completed. deletedSnapshots={DeletedSnapshots}, updatedIndexes={UpdatedIndexes}.",
            deletedCount,
            updatedIndexCount);
        return (deletedCount, updatedIndexCount);
    }

    private static bool IsValidBackupSnapshot(BackupSnapshotDocument snapshot)
    {
        return !string.IsNullOrWhiteSpace(snapshot.RootPrefix) && snapshot.TimestampUnixSeconds > 0;
    }

    private static bool IsValidMirrorSnapshot(MirrorSnapshotDocument snapshot)
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

    private static bool BackupSnapshotsAreEqual(
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
                leftSnapshot.TimestampUnixSeconds != rightSnapshot.TimestampUnixSeconds)
            {
                return false;
            }
        }

        return true;
    }

    private static bool MirrorSnapshotsAreEqual(
        IReadOnlyList<MirrorSnapshotDocument> left,
        IReadOnlyList<MirrorSnapshotDocument> right)
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
                leftSnapshot.TimestampUnixSeconds != rightSnapshot.TimestampUnixSeconds)
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

    private static MirrorRegistryDocument ParseOrCreateMirrorRegistry(string? json)
    {
        if (string.IsNullOrWhiteSpace(json))
        {
            return new MirrorRegistryDocument();
        }

        if (!StorageIndexDocuments.TryDeserialize<MirrorRegistryDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn("Mirror registry is invalid JSON. Rebuilding from discovered state.");
            return new MirrorRegistryDocument();
        }

        parsed.MirrorRoots = (parsed.MirrorRoots ?? [])
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .Select(value => value.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToList();
        parsed.IndexKeys = (parsed.IndexKeys ?? [])
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .Select(value => value.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToList();
        return parsed;
    }
}
