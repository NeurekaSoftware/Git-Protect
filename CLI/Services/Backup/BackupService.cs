using System.Security.Cryptography;
using System.Text;
using CLI.Configuration;
using CLI.Services.Git;
using CLI.Services.Paths;
using CLI.Services.Providers;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class BackupService
{
    private const string BackupMarkerName = ".backup-root";

    private readonly BackupProviderClientFactory _providerFactory;
    private readonly IGitRepositoryService _gitRepositoryService;
    private readonly IObjectStorageService _objectStorageService;
    private readonly string _workingRoot;

    public BackupService(
        BackupProviderClientFactory providerFactory,
        IGitRepositoryService gitRepositoryService,
        IObjectStorageService objectStorageService,
        string workingRoot)
    {
        _providerFactory = providerFactory;
        _gitRepositoryService = gitRepositoryService;
        _objectStorageService = objectStorageService;
        _workingRoot = workingRoot;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        foreach (var backup in settings.Backups.Where(backup => backup?.Enabled != false))
        {
            if (backup is null || string.IsNullOrWhiteSpace(backup.Provider) || string.IsNullOrWhiteSpace(backup.Credential))
            {
                continue;
            }

            try
            {
                var providerClient = _providerFactory.Resolve(backup.Provider);
                var credential = settings.Credentials[backup.Credential];
                var discoveredRepositories = await providerClient.ListOwnedRepositoriesAsync(backup, credential, cancellationToken);
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

                        await _gitRepositoryService.SyncBareRepositoryAsync(
                            repository.CloneUrl,
                            localPath,
                            gitCredential,
                            force: true,
                            includeLfs: backup.Lfs == true,
                            cancellationToken);

                        await _objectStorageService.UploadDirectoryAsync(localPath, backupPrefix, cancellationToken);
                        await _objectStorageService.UploadTextAsync(
                            $"{backupPrefix}/{BackupMarkerName}",
                            $"{repository.CloneUrl}\n{timestamp:O}",
                            cancellationToken);

                        Console.WriteLine($"Backed up '{repository.CloneUrl}' to '{backupPrefix}'.");
                    }
                    catch (Exception exception)
                    {
                        Console.Error.WriteLine($"Backup failed for repository '{repository.CloneUrl}': {exception.Message}");
                    }
                }
            }
            catch (Exception exception)
            {
                Console.Error.WriteLine($"Backup provider '{backup.Provider}' failed: {exception.Message}");
            }
        }
    }

    private static string ComputeDeterministicFolderName(string value)
    {
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(value));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }
}
