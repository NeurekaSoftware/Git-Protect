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
        AppLogger.Info("Mirror run started. enabledMirrors={EnabledMirrorCount}", enabledMirrors.Length);

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var mirrorRegistryObjectKey = StorageKeyBuilder.BuildMirrorRegistryObjectKey();

        var prunedMirrors = 0;

        var mirrorRegistry = await LoadMirrorRegistryAsync(objectStorageService, mirrorRegistryObjectKey, cancellationToken);
        var knownMirrorPrefixes = new HashSet<string>(mirrorRegistry.MirrorRoots, StringComparer.Ordinal);
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);

        AppLogger.Debug(
            "Mirror storage target configured. endpoint={Endpoint}, bucket={Bucket}, region={Region}, pruneOrphanedMirrors={PruneOrphanedMirrors}.",
            settings.Storage.Endpoint,
            settings.Storage.Bucket,
            settings.Storage.Region,
            settings.Storage.PruneOrphanedMirrors);

        foreach (var mirror in enabledMirrors)
        {
            if (mirror is null || string.IsNullOrWhiteSpace(mirror.Url))
            {
                AppLogger.Warn("Skipping mirror entry because url is missing.");
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
                AppLogger.Info("Mirror sync started. repository={RepositoryUrl}", mirror.Url);
                AppLogger.Debug(
                    "Mirror working paths resolved. repository={RepositoryUrl}, localPath={LocalPath}, targetPrefix={TargetPrefix}.",
                    mirror.Url,
                    localPath,
                    mirrorPrefix);

                await _gitRepositoryService.SyncBareRepositoryAsync(
                    mirror.Url,
                    localPath,
                    credential,
                    mirror.Force == true,
                    mirror.Lfs == true,
                    cancellationToken);

                var archiveObjectKey = $"{mirrorPrefix}/{ArchiveObjectName}";
                await objectStorageService.UploadDirectoryAsTarGzAsync(
                    localPath,
                    archiveObjectKey,
                    cancellationToken);
                await objectStorageService.UploadTextAsync(
                    $"{mirrorPrefix}/{MirrorMarkerName}",
                    mirror.Url,
                    cancellationToken);

                AppLogger.Info(
                    "Mirror sync completed. repository={RepositoryUrl}, destination={MirrorPrefix}.",
                    mirror.Url,
                    mirrorPrefix);
            }
            catch (Exception exception)
            {
                AppLogger.Error(
                    exception,
                    "Mirror sync failed. repository={RepositoryUrl}, error={ErrorMessage}",
                    mirror.Url,
                    exception.Message);
            }
        }

        var mirrorRegistryChanged = false;

        if (settings.Storage.PruneOrphanedMirrors == true)
        {
            AppLogger.Info("Pruning orphaned mirrors is enabled.");
            foreach (var mirrorPrefix in knownMirrorPrefixes)
            {
                if (activeMirrorPrefixes.Contains(mirrorPrefix))
                {
                    continue;
                }

                AppLogger.Info("Pruning orphaned mirror prefix={MirrorPrefix}.", mirrorPrefix);
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
            AppLogger.Debug("Pruning orphaned mirrors is disabled.");
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
            "Mirror run completed. prunedMirrors={PrunedMirrors}.",
            prunedMirrors);
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
