package forge

import (
	"fmt"
	"strings"

	"github.com/neurekadev/git-backup/internal/config"
)

// Factory resolves the provider clients by name.
type Factory struct {
	clients map[string]RepositoryProviderClient
}

// NewFactory registers the provider clients, keyed case-insensitively by
// provider name. Settings validation accepts every provider the config lists,
// so one listed there but never registered here would pass validation at
// startup and only fail once a run reached it — fail immediately instead,
// while the two lists can still be reconciled.
func NewFactory(clients ...RepositoryProviderClient) (*Factory, error) {
	byName := make(map[string]RepositoryProviderClient, len(clients))
	for _, client := range clients {
		byName[strings.ToLower(client.Provider())] = client
	}

	var missing []string
	for _, provider := range []string{config.ProviderGitHub, config.ProviderGitLab, config.ProviderForgejo} {
		if _, registered := byName[provider]; !registered {
			missing = append(missing, provider)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no provider client registered for: %s.", strings.Join(missing, ", "))
	}

	return &Factory{clients: byName}, nil
}

// Resolve returns the client for a provider name.
func (f *Factory) Resolve(provider string) (RepositoryProviderClient, error) {
	if client, ok := f.clients[strings.ToLower(provider)]; ok {
		return client, nil
	}
	return nil, fmt.Errorf("no provider client registered for '%s'.", provider)
}

// TryResolveMetadata returns the provider's project-metadata client, or nil
// when the provider is unregistered or does not support project metadata.
func (f *Factory) TryResolveMetadata(provider string) ProjectMetadataProviderClient {
	client, ok := f.clients[strings.ToLower(provider)]
	if !ok {
		return nil
	}
	if metadata, ok := client.(ProjectMetadataProviderClient); ok {
		return metadata
	}
	return nil
}

// NewDefaultFactory registers the built-in provider clients.
func NewDefaultFactory() (*Factory, error) {
	return NewFactory(NewGitHubClient(), NewGitLabClient(), NewForgejoClient())
}
