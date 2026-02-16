using CLI.Configuration;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class RetentionService
{
    private const string BackupMarkerName = ".backup-root";

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
            AppLogger.Info("retention: disabled. Keeping backups forever.");
            return;
        }

        AppLogger.Info($"retention: run started with retention='{retentionDays}' day(s).");

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var cutoff = DateTimeOffset.UtcNow.AddDays(-retentionDays.Value);
        AppLogger.Info($"retention: listing backup markers before cutoff '{cutoff:O}'.");
        var backupKeys = await objectStorageService.ListObjectKeysAsync("backups/", cancellationToken);
        AppLogger.Debug($"retention: loaded {backupKeys.Count} key(s) under backups/.");

        var snapshots = backupKeys
            .Where(key => key.EndsWith($"/{BackupMarkerName}", StringComparison.Ordinal))
            .Select(ParseSnapshot)
            .Where(snapshot => snapshot is not null)
            .Cast<BackupSnapshot>()
            .ToList();
        AppLogger.Debug($"retention: parsed {snapshots.Count} snapshot marker(s).");

        var snapshotsByRepository = snapshots
            .GroupBy(snapshot => snapshot.RepositoryIdentity, StringComparer.Ordinal)
            .ToList();
        AppLogger.Debug($"retention: grouped snapshots into {snapshotsByRepository.Count} repository bucket(s).");

        var deletedCount = 0;

        foreach (var repositorySnapshots in snapshotsByRepository)
        {
            var expired = repositorySnapshots
                .Where(snapshot => snapshot.Timestamp < cutoff)
                .ToList();
            AppLogger.Debug(
                $"retention: repository '{repositorySnapshots.Key}' has {expired.Count} expired snapshot(s).");

            foreach (var snapshot in expired)
            {
                await objectStorageService.DeletePrefixAsync(snapshot.RootPrefix, cancellationToken);
                deletedCount++;
                AppLogger.Info($"retention: deleted expired backup '{snapshot.RootPrefix}'.");
            }
        }

        AppLogger.Info($"retention: run completed. Deleted {deletedCount} snapshot(s).");
    }

    private static BackupSnapshot? ParseSnapshot(string markerKey)
    {
        var rootPrefix = markerKey[..^($"/{BackupMarkerName}".Length)];
        var segments = rootPrefix.Split('/', StringSplitOptions.RemoveEmptyEntries);

        if (segments.Length < 7)
        {
            return null;
        }

        if (!segments[0].Equals("backups", StringComparison.Ordinal))
        {
            return null;
        }

        if (!long.TryParse(segments[4], out var unixTimestamp))
        {
            return null;
        }

        var repositoryIdentity = string.Join('/', segments.Skip(5));

        return new BackupSnapshot
        {
            RootPrefix = rootPrefix,
            RepositoryIdentity = repositoryIdentity,
            Timestamp = DateTimeOffset.FromUnixTimeSeconds(unixTimestamp)
        };
    }

    private sealed class BackupSnapshot
    {
        public required string RootPrefix { get; init; }

        public required string RepositoryIdentity { get; init; }

        public required DateTimeOffset Timestamp { get; init; }
    }
}
