using CLI.Configuration;
using CLI.Configuration.Models;
using CLI.Services.Git;
using CLI.Services.Paths;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class MirrorService
{
    private const string MirrorMarkerName = ".mirror-root";

    private readonly IGitRepositoryService _gitRepositoryService;
    private readonly Func<StorageConfig, IObjectStorageService> _objectStorageServiceFactory;
    private readonly string _workingRoot;

    public MirrorService(
        IGitRepositoryService gitRepositoryService,
        Func<StorageConfig, IObjectStorageService> objectStorageServiceFactory,
        string workingRoot)
    {
        _gitRepositoryService = gitRepositoryService;
        _objectStorageServiceFactory = objectStorageServiceFactory;
        _workingRoot = workingRoot;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);

        foreach (var mirror in settings.Mirrors.Where(mirror => mirror?.Enabled != false))
        {
            if (mirror is null || string.IsNullOrWhiteSpace(mirror.Url))
            {
                continue;
            }

            try
            {
                var pathInfo = RepositoryPathParser.Parse(mirror.Url);
                var mirrorPrefix = StorageKeyBuilder.BuildMirrorPrefix(pathInfo);
                activeMirrorPrefixes.Add(mirrorPrefix);

                var localPath = BuildLocalPathFromPrefix(mirrorPrefix);
                var credential = string.IsNullOrWhiteSpace(mirror.Credential)
                    ? null
                    : CredentialResolver.ResolveGitCredential(settings.Credentials[mirror.Credential]);

                await _gitRepositoryService.SyncBareRepositoryAsync(
                    mirror.Url,
                    localPath,
                    credential,
                    mirror.Force == true,
                    mirror.Lfs == true,
                    cancellationToken);

                await objectStorageService.UploadDirectoryAsync(localPath, mirrorPrefix, cancellationToken);
                await objectStorageService.UploadTextAsync($"{mirrorPrefix}/{MirrorMarkerName}", mirror.Url, cancellationToken);

                Console.WriteLine($"Mirrored '{mirror.Url}' to '{mirrorPrefix}'.");
            }
            catch (Exception exception)
            {
                Console.Error.WriteLine($"Mirror failed for '{mirror.Url}': {exception.Message}");
            }
        }

        if (settings.Storage.PruneOrphanedMirrors == true)
        {
            await PruneOrphanedMirrorsAsync(objectStorageService, activeMirrorPrefixes, cancellationToken);
        }
    }

    private async Task PruneOrphanedMirrorsAsync(
        IObjectStorageService objectStorageService,
        HashSet<string> activeMirrorPrefixes,
        CancellationToken cancellationToken)
    {
        var mirrorKeys = await objectStorageService.ListObjectKeysAsync("mirrors/", cancellationToken);
        var existingMirrorRoots = mirrorKeys
            .Where(key => key.EndsWith($"/{MirrorMarkerName}", StringComparison.Ordinal))
            .Select(key => key[..^($"/{MirrorMarkerName}".Length)])
            .Distinct(StringComparer.Ordinal)
            .ToArray();

        foreach (var mirrorRoot in existingMirrorRoots)
        {
            if (activeMirrorPrefixes.Contains(mirrorRoot))
            {
                continue;
            }

            await objectStorageService.DeletePrefixAsync(mirrorRoot, cancellationToken);
            Console.WriteLine($"Pruned orphaned mirror '{mirrorRoot}'.");
        }
    }

    private string BuildLocalPathFromPrefix(string mirrorPrefix)
    {
        var localPath = _workingRoot;

        foreach (var segment in mirrorPrefix.Split('/', StringSplitOptions.RemoveEmptyEntries))
        {
            localPath = Path.Combine(localPath, segment);
        }

        return localPath;
    }
}
