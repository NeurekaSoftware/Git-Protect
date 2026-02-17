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
        var archiveModeEnabled = settings.Storage.ArchiveMode == true;
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

                        if (archiveModeEnabled)
                        {
                            var archiveObjectKey = $"{backupPrefix}/{ArchiveObjectName}";
                            AppLogger.Info($"backup: uploading git archive for '{repository.CloneUrl}'.");
                            await objectStorageService.UploadDirectoryAsTarGzAsync(localPath, archiveObjectKey, cancellationToken);
                        }
                        else
                        {
                            AppLogger.Info($"backup: uploading git objects for '{repository.CloneUrl}'.");
                            await objectStorageService.UploadDirectoryAsync(localPath, backupPrefix, cancellationToken);
                        }
                        AppLogger.Info($"backup: writing marker for '{repository.CloneUrl}'.");
                        await objectStorageService.UploadTextAsync(
                            $"{backupPrefix}/{BackupMarkerName}",
                            $"{repository.CloneUrl}\n{timestamp:O}",
                            cancellationToken);

                        AppLogger.Info($"backup: completed '{repository.CloneUrl}' to '{backupPrefix}'.");
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

        AppLogger.Info("backup: run completed.");
    }

    private static string ComputeDeterministicFolderName(string value)
    {
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(value));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }
}
