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

        var mirrorRegistry = await LoadMirrorRegistryAsync(objectStorageService, mirrorRegistryObjectKey, cancellationToken);
        var knownMirrorIndexKeys = new HashSet<string>(mirrorRegistry.IndexKeys, StringComparer.Ordinal);
        var mirrorRegistryChanged = false;

        AppLogger.Debug(
            "Mirror storage target configured. endpoint={Endpoint}, bucket={Bucket}, region={Region}.",
            settings.Storage.Endpoint,
            settings.Storage.Bucket,
            settings.Storage.Region);

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

                var localPath = BuildLocalPathFromPrefix(mirrorPrefix);
                var credential = string.IsNullOrWhiteSpace(mirror.Credential)
                    ? null
                    : CredentialResolver.ResolveGitCredential(settings.Credentials[mirror.Credential]);
                var mirrorIndexContent = await objectStorageService.GetTextIfExistsAsync(mirrorIndexObjectKey, cancellationToken);

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
                    force: false,
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

        if (mirrorRegistryChanged)
        {
            mirrorRegistry.IndexKeys = knownMirrorIndexKeys.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                mirrorRegistryObjectKey,
                StorageIndexDocuments.Serialize(mirrorRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            "Mirror run completed. trackedMirrorIndexes={MirrorIndexCount}.",
            knownMirrorIndexKeys.Count);
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
