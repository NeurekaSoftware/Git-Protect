using CLI.Configuration.Models;

namespace CLI.Services.Providers;

public interface IBackupProviderClient
{
    string Provider { get; }

    Task<IReadOnlyList<DiscoveredRepository>> ListOwnedRepositoriesAsync(
        BackupJobConfig backup,
        CredentialConfig credential,
        CancellationToken cancellationToken);
}
