namespace CLI.Services.Paths;

public static class StorageKeyBuilder
{
    private const string DefaultIndexPrefix = "indexes";

    public static string BuildBackupRepositoryIdentity(string provider, RepositoryPathInfo repository)
    {
        var segments = new List<string>
        {
            provider.Trim().ToLowerInvariant(),
            repository.BaseDomain
        };

        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string BuildMirrorPrefix(RepositoryPathInfo repository)
    {
        var segments = new List<string> { "mirrors", repository.FullDomain };
        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string BuildMirrorRepositoryIdentity(RepositoryPathInfo repository)
    {
        var segments = new List<string> { repository.FullDomain };
        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string BuildBackupPrefix(string provider, RepositoryPathInfo repository)
    {
        var segments = new List<string>
        {
            "backups",
            provider.Trim().ToLowerInvariant()
        };

        segments.AddRange(repository.Hierarchy);
        return string.Join('/', segments);
    }

    public static string BuildBackupIndexRegistryObjectKey()
    {
        var normalizedIndexPrefix = EnsurePrefix(DefaultIndexPrefix);
        return $"{normalizedIndexPrefix}backups/registry.json".Trim('/');
    }

    public static string BuildBackupRepositoryIndexObjectKey(
        string provider,
        RepositoryPathInfo repository)
    {
        var normalizedIndexPrefix = EnsurePrefix(DefaultIndexPrefix);
        var repositoryIdentity = BuildBackupRepositoryIdentity(provider, repository);
        return $"{normalizedIndexPrefix}backups/{repositoryIdentity}/index.json".Trim('/');
    }

    public static string BuildMirrorRegistryObjectKey()
    {
        var normalizedIndexPrefix = EnsurePrefix(DefaultIndexPrefix);
        return $"{normalizedIndexPrefix}mirrors/registry.json".Trim('/');
    }

    public static string BuildMirrorRepositoryIndexObjectKey(RepositoryPathInfo repository)
    {
        var normalizedIndexPrefix = EnsurePrefix(DefaultIndexPrefix);
        var repositoryIdentity = BuildMirrorRepositoryIdentity(repository);
        return $"{normalizedIndexPrefix}mirrors/{repositoryIdentity}/index.json".Trim('/');
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
