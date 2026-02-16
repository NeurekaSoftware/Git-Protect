using CLI.Configuration;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class RetentionService
{
    private const string BackupMarkerName = ".backup-root";

    private readonly IObjectStorageService _objectStorageService;

    public RetentionService(IObjectStorageService objectStorageService)
    {
        _objectStorageService = objectStorageService;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        var retentionDays = settings.Storage.Retention;
        if (retentionDays is null || retentionDays <= 0)
        {
            Console.WriteLine("Retention is disabled. Keeping backups forever.");
            return;
        }

        var cutoff = DateTimeOffset.UtcNow.AddDays(-retentionDays.Value);
        var backupKeys = await _objectStorageService.ListObjectKeysAsync("backups/", cancellationToken);

        var snapshots = backupKeys
            .Where(key => key.EndsWith($"/{BackupMarkerName}", StringComparison.Ordinal))
            .Select(ParseSnapshot)
            .Where(snapshot => snapshot is not null)
            .Cast<BackupSnapshot>()
            .ToList();

        var snapshotsByRepository = snapshots
            .GroupBy(snapshot => snapshot.RepositoryIdentity, StringComparer.Ordinal)
            .ToList();

        foreach (var repositorySnapshots in snapshotsByRepository)
        {
            var expired = repositorySnapshots
                .Where(snapshot => snapshot.Timestamp < cutoff)
                .ToList();

            foreach (var snapshot in expired)
            {
                await _objectStorageService.DeletePrefixAsync(snapshot.RootPrefix, cancellationToken);
                Console.WriteLine($"Deleted expired backup '{snapshot.RootPrefix}'.");
            }
        }
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
