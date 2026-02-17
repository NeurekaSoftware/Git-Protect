using System.Text.Json;

namespace CLI.Services.Backup;

internal static class StorageIndexDocuments
{
    private static readonly JsonSerializerOptions SerializerOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        WriteIndented = false
    };

    public static string Serialize<TDocument>(TDocument document)
    {
        return JsonSerializer.Serialize(document, SerializerOptions);
    }

    public static bool TryDeserialize<TDocument>(string json, out TDocument? document)
    {
        try
        {
            document = JsonSerializer.Deserialize<TDocument>(json, SerializerOptions);
            return document is not null;
        }
        catch
        {
            document = default;
            return false;
        }
    }
}

internal sealed class BackupIndexRegistryDocument
{
    public List<string> IndexKeys { get; set; } = [];
}

internal sealed class BackupRepositoryIndexDocument
{
    public string RepositoryIdentity { get; set; } = string.Empty;

    public List<BackupSnapshotDocument> Snapshots { get; set; } = [];
}

internal sealed class BackupSnapshotDocument
{
    public string RootPrefix { get; set; } = string.Empty;

    public long TimestampUnixSeconds { get; set; }
}

internal sealed class MirrorRegistryDocument
{
    public List<string> IndexKeys { get; set; } = [];
}

internal sealed class MirrorRepositoryIndexDocument
{
    public string RepositoryIdentity { get; set; } = string.Empty;

    public List<MirrorSnapshotDocument> Snapshots { get; set; } = [];
}

internal sealed class MirrorSnapshotDocument
{
    public string RootPrefix { get; set; } = string.Empty;

    public long TimestampUnixSeconds { get; set; }
}
