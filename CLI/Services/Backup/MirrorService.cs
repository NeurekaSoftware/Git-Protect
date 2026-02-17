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
    private const string ArchiveObjectNameSuffix = "_repo.tar.gz";

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
        var prunedMirrorIndexes = 0;

        var mirrorRegistry = await LoadMirrorRegistryAsync(objectStorageService, mirrorRegistryObjectKey, cancellationToken);
        var knownMirrorPrefixes = new HashSet<string>(mirrorRegistry.MirrorRoots, StringComparer.Ordinal);
        var knownMirrorIndexKeys = new HashSet<string>(mirrorRegistry.IndexKeys, StringComparer.Ordinal);
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);
        var activeMirrorIndexKeys = new HashSet<string>(StringComparer.Ordinal);
        var mirrorRegistryChanged = false;

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
                var mirrorRepositoryIdentity = StorageKeyBuilder.BuildMirrorRepositoryIdentity(pathInfo);
                var mirrorIndexObjectKey = StorageKeyBuilder.BuildMirrorRepositoryIndexObjectKey(pathInfo);
                activeMirrorPrefixes.Add(mirrorPrefix);

                var localPath = BuildLocalPathFromPrefix(mirrorPrefix);
                var credential = string.IsNullOrWhiteSpace(mirror.Credential)
                    ? null
                    : CredentialResolver.ResolveGitCredential(settings.Credentials[mirror.Credential]);
                var mirrorIndexContent = await objectStorageService.GetTextIfExistsAsync(mirrorIndexObjectKey, cancellationToken);
                if (!string.IsNullOrWhiteSpace(mirrorIndexContent))
                {
                    activeMirrorIndexKeys.Add(mirrorIndexObjectKey);
                }

                var mirrorIndexDocument = ParseOrCreateMirrorRepositoryIndex(mirrorIndexContent, mirrorRepositoryIdentity);
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

                var timestamp = DateTimeOffset.UtcNow;
                var archiveObjectKey = $"{mirrorPrefix}/{BuildArchiveObjectName(timestamp)}";
                await objectStorageService.UploadDirectoryAsTarGzAsync(
                    localPath,
                    archiveObjectKey,
                    cancellationToken);

                mirrorIndexDocument.Snapshots = mirrorIndexDocument.Snapshots
                    .Where(IsValidSnapshot)
                    .Where(snapshot => !string.Equals(snapshot.RootPrefix, archiveObjectKey, StringComparison.Ordinal))
                    .ToList();
                mirrorIndexDocument.Snapshots.Add(new MirrorSnapshotDocument
                {
                    RootPrefix = archiveObjectKey,
                    TimestampUnixSeconds = timestamp.ToUnixTimeSeconds()
                });

                var updatedMirrorIndexContent = StorageIndexDocuments.Serialize(mirrorIndexDocument);
                if (!string.Equals(mirrorIndexContent, updatedMirrorIndexContent, StringComparison.Ordinal))
                {
                    await objectStorageService.UploadTextAsync(
                        mirrorIndexObjectKey,
                        updatedMirrorIndexContent,
                        cancellationToken);
                }

                activeMirrorIndexKeys.Add(mirrorIndexObjectKey);
                if (knownMirrorIndexKeys.Add(mirrorIndexObjectKey))
                {
                    mirrorRegistryChanged = true;
                }

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

            var staleMirrorIndexKeys = knownMirrorIndexKeys
                .Where(indexKey => !activeMirrorIndexKeys.Contains(indexKey))
                .ToArray();
            if (staleMirrorIndexKeys.Length > 0)
            {
                AppLogger.Info(
                    "Pruning orphaned mirror indexes. indexCount={MirrorIndexCount}.",
                    staleMirrorIndexKeys.Length);
                await objectStorageService.DeleteObjectsAsync(staleMirrorIndexKeys, cancellationToken);
                prunedMirrorIndexes += staleMirrorIndexKeys.Length;
            }

            if (!knownMirrorPrefixes.SetEquals(activeMirrorPrefixes) ||
                !knownMirrorIndexKeys.SetEquals(activeMirrorIndexKeys))
            {
                mirrorRegistryChanged = true;
                knownMirrorPrefixes = new HashSet<string>(activeMirrorPrefixes, StringComparer.Ordinal);
                knownMirrorIndexKeys = new HashSet<string>(activeMirrorIndexKeys, StringComparer.Ordinal);
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

            foreach (var activeMirrorIndexKey in activeMirrorIndexKeys)
            {
                if (knownMirrorIndexKeys.Add(activeMirrorIndexKey))
                {
                    mirrorRegistryChanged = true;
                }
            }
        }

        if (mirrorRegistryChanged)
        {
            mirrorRegistry.MirrorRoots = knownMirrorPrefixes.OrderBy(value => value, StringComparer.Ordinal).ToList();
            mirrorRegistry.IndexKeys = knownMirrorIndexKeys.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                mirrorRegistryObjectKey,
                StorageIndexDocuments.Serialize(mirrorRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            "Mirror run completed. prunedMirrors={PrunedMirrors}, prunedMirrorIndexes={PrunedMirrorIndexes}.",
            prunedMirrors,
            prunedMirrorIndexes);
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

    private static MirrorRepositoryIndexDocument ParseOrCreateMirrorRepositoryIndex(string? json, string repositoryIdentity)
    {
        MirrorRepositoryIndexDocument document;

        if (string.IsNullOrWhiteSpace(json))
        {
            document = new MirrorRepositoryIndexDocument();
        }
        else if (!StorageIndexDocuments.TryDeserialize<MirrorRepositoryIndexDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn(
                "Mirror index is invalid JSON. Rebuilding index for repository={RepositoryIdentity}.",
                repositoryIdentity);
            document = new MirrorRepositoryIndexDocument();
        }
        else
        {
            document = parsed;
        }

        document.RepositoryIdentity = repositoryIdentity;
        document.Snapshots = document.Snapshots ?? [];
        return document;
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

    private static string BuildArchiveObjectName(DateTimeOffset timestamp)
    {
        return $"{timestamp.ToUnixTimeSeconds()}{ArchiveObjectNameSuffix}";
    }

    private static bool IsValidSnapshot(MirrorSnapshotDocument snapshot)
    {
        return !string.IsNullOrWhiteSpace(snapshot.RootPrefix) && snapshot.TimestampUnixSeconds > 0;
    }
}
