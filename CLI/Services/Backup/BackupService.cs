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
        AppLogger.Info("Backup run started. enabledJobs={EnabledJobCount}", enabledBackups.Length);

        var objectStorageService = _objectStorageServiceFactory(settings.Storage);
        var backupRegistryObjectKey = StorageKeyBuilder.BuildBackupIndexRegistryObjectKey();

        var uploadsSkipped = 0;
        var markersSkipped = 0;

        var backupRegistry = await LoadBackupRegistryAsync(objectStorageService, backupRegistryObjectKey, cancellationToken);
        var knownIndexKeys = new HashSet<string>(backupRegistry.IndexKeys, StringComparer.Ordinal);
        var backupRegistryChanged = false;

        AppLogger.Debug(
            "Backup storage target configured. endpoint={Endpoint}, bucket={Bucket}, region={Region}.",
            settings.Storage.Endpoint,
            settings.Storage.Bucket,
            settings.Storage.Region);

        foreach (var backup in enabledBackups)
        {
            if (backup is null || string.IsNullOrWhiteSpace(backup.Provider) || string.IsNullOrWhiteSpace(backup.Credential))
            {
                AppLogger.Warn("Skipping backup job because provider or credential is missing.");
                continue;
            }

            try
            {
                AppLogger.Info("Backup provider job started. provider={Provider}", backup.Provider);
                var providerClient = _providerFactory.Resolve(backup.Provider);
                var credential = settings.Credentials[backup.Credential];
                AppLogger.Info("Loading repositories from provider={Provider}.", backup.Provider);
                var discoveredRepositories = await providerClient.ListOwnedRepositoriesAsync(backup, credential, cancellationToken);
                AppLogger.Info(
                    "Provider repository discovery completed. provider={Provider}, repositories={RepositoryCount}.",
                    backup.Provider,
                    discoveredRepositories.Count);
                var gitCredential = CredentialResolver.ResolveGitCredential(credential);

                foreach (var repository in discoveredRepositories)
                {
                    cancellationToken.ThrowIfCancellationRequested();

                    try
                    {
                        var pathInfo = RepositoryPathParser.Parse(repository.CloneUrl);
                        var repositoryIdentity = StorageKeyBuilder.BuildBackupRepositoryIdentity(backup.Provider, pathInfo);
                        var backupIndexObjectKey = StorageKeyBuilder.BuildBackupRepositoryIndexObjectKey(backup.Provider, pathInfo);
                        var indexContent = await objectStorageService.GetTextIfExistsAsync(backupIndexObjectKey, cancellationToken);
                        var indexDocument = ParseOrCreateRepositoryIndex(indexContent, repositoryIdentity);
                        var latestArchiveSha256 = GetLatestSnapshotSha256(indexDocument);
                        var timestamp = DateTimeOffset.UtcNow;
                        var backupPrefix = StorageKeyBuilder.BuildBackupPrefix(backup.Provider, pathInfo, timestamp);
                        var archiveObjectKey = $"{backupPrefix}/{ArchiveObjectName}";
                        var localPath = Path.Combine(_workingRoot, "backups", ComputeDeterministicFolderName($"{backup.Provider}:{repository.CloneUrl}"));
                        AppLogger.Info("Repository backup started. repository={RepositoryUrl}", repository.CloneUrl);
                        AppLogger.Debug(
                            "Repository working paths resolved. repository={RepositoryUrl}, localPath={LocalPath}, targetPrefix={TargetPrefix}.",
                            repository.CloneUrl,
                            localPath,
                            backupPrefix);

                        await _gitRepositoryService.SyncBareRepositoryAsync(
                            repository.CloneUrl,
                            localPath,
                            gitCredential,
                            force: true,
                            includeLfs: backup.Lfs == true,
                            cancellationToken);

                        var archiveUploadResult = await objectStorageService.UploadDirectoryAsTarGzAsync(
                            localPath,
                            archiveObjectKey,
                            cancellationToken,
                            latestArchiveSha256,
                            useHeadHashCheck: false);
                        if (archiveUploadResult.Uploaded)
                        {
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

                            var updatedIndexContent = StorageIndexDocuments.Serialize(indexDocument);
                            if (!string.Equals(indexContent, updatedIndexContent, StringComparison.Ordinal))
                            {
                                await objectStorageService.UploadTextAsync(
                                    backupIndexObjectKey,
                                    updatedIndexContent,
                                    cancellationToken);
                            }

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
                            await objectStorageService.UploadTextAsync(
                                $"{backupPrefix}/{BackupMarkerName}",
                                $"{repository.CloneUrl}\n{timestamp:O}\nsha256={archiveUploadResult.Sha256}",
                                cancellationToken);
                        }
                        else
                        {
                            markersSkipped++;
                            AppLogger.Debug(
                                "Marker file skipped because repository content is unchanged. repository={RepositoryUrl}",
                                repository.CloneUrl);
                        }

                        AppLogger.Info(
                            "Repository backup completed. repository={RepositoryUrl}, destination={BackupPrefix}, archiveUploaded={ArchiveUploaded}.",
                            repository.CloneUrl,
                            backupPrefix,
                            archiveUploadResult.Uploaded);
                    }
                    catch (Exception exception)
                    {
                        AppLogger.Error(
                            exception,
                            "Repository backup failed. repository={RepositoryUrl}, error={ErrorMessage}",
                            repository.CloneUrl,
                            exception.Message);
                    }
                }

                AppLogger.Info("Backup provider job completed. provider={Provider}", backup.Provider);
            }
            catch (Exception exception)
            {
                AppLogger.Error(
                    exception,
                    "Backup provider job failed. provider={Provider}, error={ErrorMessage}",
                    backup.Provider,
                    exception.Message);
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
            "Backup run completed. uploadsSkipped={UploadsSkipped}, markersSkipped={MarkersSkipped}.",
            uploadsSkipped,
            markersSkipped);
    }

    private static BackupIndexRegistryDocument ParseOrCreateBackupRegistry(string? json)
    {
        if (string.IsNullOrWhiteSpace(json))
        {
            return new BackupIndexRegistryDocument();
        }

        if (!StorageIndexDocuments.TryDeserialize<BackupIndexRegistryDocument>(json, out var parsed) || parsed is null)
        {
            AppLogger.Warn("Backup index registry is invalid JSON. Rebuilding from discovered state.");
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
            AppLogger.Warn(
                "Repository backup index is invalid JSON. Rebuilding index for repository={RepositoryIdentity}.",
                repositoryIdentity);
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

    private static string? GetLatestSnapshotSha256(BackupRepositoryIndexDocument indexDocument)
    {
        return indexDocument.Snapshots
            .Where(IsValidSnapshot)
            .OrderByDescending(snapshot => snapshot.TimestampUnixSeconds)
            .Select(snapshot => snapshot.ArchiveSha256)
            .FirstOrDefault(value => !string.IsNullOrWhiteSpace(value));
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
