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
        var mirrorRegistryObjectKey = StorageKeyBuilder.BuildMirrorRegistryObjectKey();

        var uploadsSkipped = 0;
        var markersSkipped = 0;
        var prunedMirrors = 0;

        var mirrorRegistry = await LoadMirrorRegistryAsync(objectStorageService, mirrorRegistryObjectKey, cancellationToken);
        var knownMirrorPrefixes = new HashSet<string>(mirrorRegistry.MirrorRoots, StringComparer.Ordinal);
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);

        AppLogger.Debug(
            $"mirror: storage endpoint='{settings.Storage.Endpoint}', bucket='{settings.Storage.Bucket}', region='{settings.Storage.Region}', pruneOrphanedMirrors='{settings.Storage.PruneOrphanedMirrors}'.");

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

                var archiveObjectKey = $"{mirrorPrefix}/{ArchiveObjectName}";
                AppLogger.Info($"mirror: uploading git archive for '{mirror.Url}'.");
                var archiveUploadResult = await objectStorageService.UploadDirectoryAsTarGzAsync(
                    localPath,
                    archiveObjectKey,
                    cancellationToken);
                if (!archiveUploadResult.Uploaded)
                {
                    uploadsSkipped++;
                }

                if (archiveUploadResult.Uploaded)
                {
                    AppLogger.Info($"mirror: writing marker for '{mirror.Url}'.");
                    await objectStorageService.UploadTextAsync(
                        $"{mirrorPrefix}/{MirrorMarkerName}",
                        $"{mirror.Url}\nsha256={archiveUploadResult.Sha256}",
                        cancellationToken);
                }
                else
                {
                    markersSkipped++;
                    AppLogger.Info($"mirror: marker skipped for '{mirror.Url}' because repository is unchanged.");
                }

                AppLogger.Info(
                    $"mirror: completed '{mirror.Url}' to '{mirrorPrefix}' (archiveUploaded='{archiveUploadResult.Uploaded}').");
            }
            catch (Exception exception)
            {
                AppLogger.Error($"mirror: '{mirror.Url}' failed: {exception.Message}", exception);
            }
        }

        var mirrorRegistryChanged = false;

        if (settings.Storage.PruneOrphanedMirrors == true)
        {
            AppLogger.Info("mirror: pruning orphaned mirrors is enabled.");
            foreach (var mirrorPrefix in knownMirrorPrefixes)
            {
                if (activeMirrorPrefixes.Contains(mirrorPrefix))
                {
                    continue;
                }

                AppLogger.Info($"mirror: pruning orphaned mirror '{mirrorPrefix}'.");
                await objectStorageService.DeletePrefixAsync(mirrorPrefix, cancellationToken);
                prunedMirrors++;
            }

            if (!knownMirrorPrefixes.SetEquals(activeMirrorPrefixes))
            {
                mirrorRegistryChanged = true;
                knownMirrorPrefixes = new HashSet<string>(activeMirrorPrefixes, StringComparer.Ordinal);
            }
        }
        else
        {
            AppLogger.Debug("mirror: pruning orphaned mirrors is disabled.");
            foreach (var activeMirrorPrefix in activeMirrorPrefixes)
            {
                if (knownMirrorPrefixes.Add(activeMirrorPrefix))
                {
                    mirrorRegistryChanged = true;
                }
            }
        }

        if (mirrorRegistryChanged)
        {
            mirrorRegistry.MirrorRoots = knownMirrorPrefixes.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                mirrorRegistryObjectKey,
                StorageIndexDocuments.Serialize(mirrorRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            $"mirror: run completed. uploadsSkipped={uploadsSkipped}, markersSkipped={markersSkipped}, prunedMirrors={prunedMirrors}.");
    }

    private static MirrorRegistryDocument ParseOrCreateMirrorRegistry(string? json)
    {
        if (string.IsNullOrWhiteSpace(json))
        {
            return new MirrorRegistryDocument();
        }

        if (!StorageIndexDocuments.TryDeserialize<MirrorRegistryDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn("mirror: mirror registry is invalid JSON. Rebuilding registry.");
            return new MirrorRegistryDocument();
        }

        parsed.MirrorRoots = (parsed.MirrorRoots ?? [])
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .Select(value => value.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToList();
        return parsed;
    }

    private static async Task<MirrorRegistryDocument> LoadMirrorRegistryAsync(
        IObjectStorageService objectStorageService,
        string mirrorRegistryObjectKey,
        CancellationToken cancellationToken)
    {
        var registryContent = await objectStorageService.GetTextIfExistsAsync(mirrorRegistryObjectKey, cancellationToken);
        return ParseOrCreateMirrorRegistry(registryContent);
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
