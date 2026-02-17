using System.Security.Cryptography;
using System.Text;
using CLI.Configuration;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Git;
using CLI.Services.Paths;
using CLI.Services.Providers;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class BackupService
{
    private const string BackupMarkerName = ".backup-root";
    private const string ArchiveObjectName = "repo.tar.gz";

    private readonly BackupProviderClientFactory _providerFactory;
    private readonly IGitRepositoryService _gitRepositoryService;
    private readonly Func<StorageConfig, IObjectStorageService> _objectStorageServiceFactory;
    private readonly string _workingRoot;

    public BackupService(
        BackupProviderClientFactory providerFactory,
        IGitRepositoryService gitRepositoryService,
        Func<StorageConfig, IObjectStorageService> objectStorageServiceFactory,
        string workingRoot)
    {
        _providerFactory = providerFactory;
        _gitRepositoryService = gitRepositoryService;
        _objectStorageServiceFactory = objectStorageServiceFactory;
        _workingRoot = workingRoot;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        var enabledBackups = settings.Backups.Where(backup => backup?.Enabled != false).ToArray();
        AppLogger.Info($"backup: run started with {enabledBackups.Length} configured job(s).");

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var backupRegistryObjectKey = StorageKeyBuilder.BuildBackupIndexRegistryObjectKey();

        var uploadsSkipped = 0;
        var markersSkipped = 0;

        var backupRegistry = await LoadBackupRegistryAsync(objectStorageService, backupRegistryObjectKey, cancellationToken);
        var knownIndexKeys = new HashSet<string>(backupRegistry.IndexKeys, StringComparer.Ordinal);
        var backupRegistryChanged = false;

        AppLogger.Debug(
            $"backup: storage endpoint='{settings.Storage.Endpoint}', bucket='{settings.Storage.Bucket}', region='{settings.Storage.Region}'.");

        foreach (var backup in enabledBackups)
        {
            if (backup is null || string.IsNullOrWhiteSpace(backup.Provider) || string.IsNullOrWhiteSpace(backup.Credential))
            {
                AppLogger.Warn("backup: skipping job with missing provider or credential.");
                continue;
            }

            try
            {
                AppLogger.Info($"backup: provider '{backup.Provider}' job started.");
                var providerClient = _providerFactory.Resolve(backup.Provider);
                var credential = settings.Credentials[backup.Credential];
                AppLogger.Info($"backup: listing repositories for provider '{backup.Provider}'.");
                var discoveredRepositories = await providerClient.ListOwnedRepositoriesAsync(backup, credential, cancellationToken);
                AppLogger.Info(
                    $"backup: provider '{backup.Provider}' returned {discoveredRepositories.Count} repository(ies).");
                var gitCredential = CredentialResolver.ResolveGitCredential(credential);

                foreach (var repository in discoveredRepositories)
                {
                    cancellationToken.ThrowIfCancellationRequested();

                    try
                    {
                        var pathInfo = RepositoryPathParser.Parse(repository.CloneUrl);
                        var timestamp = DateTimeOffset.UtcNow;
                        var backupPrefix = StorageKeyBuilder.BuildBackupPrefix(backup.Provider, pathInfo, timestamp);
                        var archiveObjectKey = $"{backupPrefix}/{ArchiveObjectName}";
                        var repositoryIdentity = StorageKeyBuilder.BuildBackupRepositoryIdentity(backup.Provider, pathInfo);
                        var backupIndexObjectKey = StorageKeyBuilder.BuildBackupRepositoryIndexObjectKey(backup.Provider, pathInfo);
                        var localPath = Path.Combine(_workingRoot, "backups", ComputeDeterministicFolderName($"{backup.Provider}:{repository.CloneUrl}"));
                        AppLogger.Info($"backup: syncing '{repository.CloneUrl}'.");
                        AppLogger.Debug($"backup: localPath='{localPath}', prefix='{backupPrefix}'.");

                        await _gitRepositoryService.SyncBareRepositoryAsync(
                            repository.CloneUrl,
                            localPath,
                            gitCredential,
                            force: true,
                            includeLfs: backup.Lfs == true,
                            cancellationToken);

                        AppLogger.Info($"backup: uploading git archive for '{repository.CloneUrl}'.");
                        var archiveUploadResult = await objectStorageService.UploadDirectoryAsTarGzAsync(
                            localPath,
                            archiveObjectKey,
                            cancellationToken);
                        if (archiveUploadResult.Uploaded)
                        {
                            var indexContent = await objectStorageService.GetTextIfExistsAsync(backupIndexObjectKey, cancellationToken);
                            var indexDocument = ParseOrCreateRepositoryIndex(indexContent, repositoryIdentity);
                            indexDocument.Snapshots = indexDocument.Snapshots
                                .Where(IsValidSnapshot)
                                .Where(snapshot => !string.Equals(snapshot.RootPrefix, backupPrefix, StringComparison.Ordinal))
                                .ToList();
                            indexDocument.Snapshots.Add(new BackupSnapshotDocument
                            {
                                RootPrefix = backupPrefix,
                                TimestampUnixSeconds = timestamp.ToUnixTimeSeconds(),
                                ArchiveSha256 = archiveUploadResult.Sha256
                            });

                            await objectStorageService.UploadTextAsync(
                                backupIndexObjectKey,
                                StorageIndexDocuments.Serialize(indexDocument),
                                cancellationToken);

                            if (knownIndexKeys.Add(backupIndexObjectKey))
                            {
                                backupRegistryChanged = true;
                            }
                        }
                        else
                        {
                            uploadsSkipped++;
                        }

                        if (archiveUploadResult.Uploaded)
                        {
                            AppLogger.Info($"backup: writing marker for '{repository.CloneUrl}'.");
                            await objectStorageService.UploadTextAsync(
                                $"{backupPrefix}/{BackupMarkerName}",
                                $"{repository.CloneUrl}\n{timestamp:O}\nsha256={archiveUploadResult.Sha256}",
                                cancellationToken);
                        }
                        else
                        {
                            markersSkipped++;
                            AppLogger.Info($"backup: marker skipped for '{repository.CloneUrl}' because repository is unchanged.");
                        }

                        AppLogger.Info(
                            $"backup: completed '{repository.CloneUrl}' to '{backupPrefix}' (archiveUploaded='{archiveUploadResult.Uploaded}').");
                    }
                    catch (Exception exception)
                    {
                        AppLogger.Error(
                            $"backup: repository '{repository.CloneUrl}' failed: {exception.Message}",
                            exception);
                    }
                }

                AppLogger.Info($"backup: provider '{backup.Provider}' job completed.");
            }
            catch (Exception exception)
            {
                AppLogger.Error($"backup: provider '{backup.Provider}' failed: {exception.Message}", exception);
            }
        }

        if (backupRegistryChanged)
        {
            backupRegistry.IndexKeys = knownIndexKeys.OrderBy(value => value, StringComparer.Ordinal).ToList();
            await objectStorageService.UploadTextAsync(
                backupRegistryObjectKey,
                StorageIndexDocuments.Serialize(backupRegistry),
                cancellationToken);
        }

        AppLogger.Info(
            $"backup: run completed. uploadsSkipped={uploadsSkipped}, markersSkipped={markersSkipped}.");
    }

    private static BackupIndexRegistryDocument ParseOrCreateBackupRegistry(string? json)
    {
        if (string.IsNullOrWhiteSpace(json))
        {
            return new BackupIndexRegistryDocument();
        }

        if (!StorageIndexDocuments.TryDeserialize<BackupIndexRegistryDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn("backup: backup index registry is invalid JSON. Rebuilding registry.");
            return new BackupIndexRegistryDocument();
        }

        parsed.IndexKeys = (parsed.IndexKeys ?? [])
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .Select(value => value.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToList();
        return parsed;
    }

    private static BackupRepositoryIndexDocument ParseOrCreateRepositoryIndex(string? json, string repositoryIdentity)
    {
        BackupRepositoryIndexDocument document;

        if (string.IsNullOrWhiteSpace(json))
        {
            document = new BackupRepositoryIndexDocument();
        }
        else if (!StorageIndexDocuments.TryDeserialize<BackupRepositoryIndexDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn($"backup: repository index for '{repositoryIdentity}' is invalid JSON. Rebuilding index.");
            document = new BackupRepositoryIndexDocument();
        }
        else
        {
            document = parsed;
        }

        document.RepositoryIdentity = repositoryIdentity;
        document.Snapshots = document.Snapshots ?? [];
        return document;
    }

    private static bool IsValidSnapshot(BackupSnapshotDocument snapshot)
    {
        return !string.IsNullOrWhiteSpace(snapshot.RootPrefix) && snapshot.TimestampUnixSeconds > 0;
    }

    private static async Task<BackupIndexRegistryDocument> LoadBackupRegistryAsync(
        IObjectStorageService objectStorageService,
        string backupRegistryObjectKey,
        CancellationToken cancellationToken)
    {
        var registryContent = await objectStorageService.GetTextIfExistsAsync(backupRegistryObjectKey, cancellationToken);
        return ParseOrCreateBackupRegistry(registryContent);
    }

    private static string ComputeDeterministicFolderName(string value)
    {
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(value));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }
}
