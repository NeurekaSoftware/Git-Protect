using CLI.Configuration.Models;

namespace CLI.Services.Providers;

public interface IRepositoryProviderClient
{
    string Provider { get; }

    Task<IReadOnlyList<DiscoveredRepository>> ListOwnedRepositoriesAsync(
        RepositoryJobConfig repository,
        CredentialConfig credential,
        CancellationToken cancellationToken);
}
