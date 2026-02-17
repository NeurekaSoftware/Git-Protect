using CLI.Configuration;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Git;
using CLI.Services.Paths;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class MirrorService
{
    private const string MirrorMarkerName = ".mirror-root";
    private const string ArchiveObjectName = "repo.tar.gz";

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
        var enabledMirrors = settings.Mirrors.Where(mirror => mirror?.Enabled != false).ToArray();
        AppLogger.Info($"mirror: run started with {enabledMirrors.Length} configured mirror(s).");

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var archiveModeEnabled = settings.Storage.ArchiveMode == true;
        AppLogger.Debug(
            $"mirror: storage endpoint='{settings.Storage.Endpoint}', bucket='{settings.Storage.Bucket}', region='{settings.Storage.Region}'.");
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);

        foreach (var mirror in enabledMirrors)
        {
            if (mirror is null || string.IsNullOrWhiteSpace(mirror.Url))
            {
                AppLogger.Warn("mirror: skipping mirror with missing url.");
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
                AppLogger.Info($"mirror: syncing '{mirror.Url}'.");
                AppLogger.Debug($"mirror: localPath='{localPath}', prefix='{mirrorPrefix}'.");

                await _gitRepositoryService.SyncBareRepositoryAsync(
                    mirror.Url,
                    localPath,
                    credential,
                    mirror.Force == true,
                    mirror.Lfs == true,
                    cancellationToken);

                if (archiveModeEnabled)
                {
                    var archiveObjectKey = $"{mirrorPrefix}/{ArchiveObjectName}";
                    AppLogger.Info($"mirror: uploading git archive for '{mirror.Url}'.");
                    await objectStorageService.UploadDirectoryAsTarGzAsync(localPath, archiveObjectKey, cancellationToken);
                }
                else
                {
                    AppLogger.Info($"mirror: uploading git objects for '{mirror.Url}'.");
                    await objectStorageService.UploadDirectoryAsync(localPath, mirrorPrefix, cancellationToken);
                }
                AppLogger.Info($"mirror: writing marker for '{mirror.Url}'.");
                await objectStorageService.UploadTextAsync($"{mirrorPrefix}/{MirrorMarkerName}", mirror.Url, cancellationToken);

                AppLogger.Info($"mirror: completed '{mirror.Url}' to '{mirrorPrefix}'.");
            }
            catch (Exception exception)
            {
                AppLogger.Error($"mirror: '{mirror.Url}' failed: {exception.Message}", exception);
            }
        }

        if (settings.Storage.PruneOrphanedMirrors == true)
        {
            AppLogger.Info("mirror: pruning orphaned mirrors is enabled.");
            await PruneOrphanedMirrorsAsync(objectStorageService, activeMirrorPrefixes, cancellationToken);
        }
        else
        {
            AppLogger.Debug("mirror: pruning orphaned mirrors is disabled.");
        }

        AppLogger.Info("mirror: run completed.");
    }

    private async Task PruneOrphanedMirrorsAsync(
        IObjectStorageService objectStorageService,
        HashSet<string> activeMirrorPrefixes,
        CancellationToken cancellationToken)
    {
        AppLogger.Info("mirror: checking for orphaned mirror prefixes.");
        var mirrorKeys = await objectStorageService.ListObjectKeysAsync("mirrors/", cancellationToken);
        var existingMirrorRoots = mirrorKeys
            .Where(key => key.EndsWith($"/{MirrorMarkerName}", StringComparison.Ordinal))
            .Select(key => key[..^($"/{MirrorMarkerName}".Length)])
            .Distinct(StringComparer.Ordinal)
            .ToArray();
        AppLogger.Debug($"mirror: found {existingMirrorRoots.Length} mirror root(s) in storage.");

        foreach (var mirrorRoot in existingMirrorRoots)
        {
            if (activeMirrorPrefixes.Contains(mirrorRoot))
            {
                continue;
            }

            AppLogger.Info($"mirror: pruning orphaned mirror '{mirrorRoot}'.");
            await objectStorageService.DeletePrefixAsync(mirrorRoot, cancellationToken);
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
