namespace CLI.Services.Providers;

public sealed class BackupProviderClientFactory
{
    private readonly Dictionary<string, IBackupProviderClient> _clients;

    public BackupProviderClientFactory(IEnumerable<IBackupProviderClient> clients)
    {
        _clients = clients.ToDictionary(client => client.Provider, client => client, StringComparer.OrdinalIgnoreCase);
    }

    public IBackupProviderClient Resolve(string provider)
    {
        if (_clients.TryGetValue(provider, out var client))
        {
            return client;
        }

        throw new InvalidOperationException($"No provider client registered for '{provider}'.");
    }
}
