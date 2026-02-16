namespace CLI.Services.Paths;

public static class StorageKeyBuilder
{
    public static string BuildMirrorPrefix(RepositoryPathInfo repository)
    {
        var segments = new List<string> { "mirrors", repository.BaseDomain };
        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string BuildBackupPrefix(string provider, RepositoryPathInfo repository, DateTimeOffset timestamp)
    {
        var segments = new List<string>
        {
            "backups",
            timestamp.ToString("yyyy"),
            timestamp.ToString("MM"),
            timestamp.ToString("dd"),
            timestamp.ToUnixTimeSeconds().ToString(),
            provider.Trim().ToLowerInvariant()
        };

        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string EnsurePrefix(string keyOrPrefix)
    {
        var value = keyOrPrefix.Trim('/');
        if (string.IsNullOrWhiteSpace(value))
        {
            return string.Empty;
        }

        return value.EndsWith('/') ? value : $"{value}/";
    }
}
