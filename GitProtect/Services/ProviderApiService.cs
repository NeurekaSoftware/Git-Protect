using System.Net.Http.Headers;
using System.Net.Http.Json;
using GitProtect.Models;

namespace GitProtect.Services;

public sealed class ProviderApiService
{
    private readonly IHttpClientFactory _httpClientFactory;

    public ProviderApiService(IHttpClientFactory httpClientFactory)
    {
        _httpClientFactory = httpClientFactory;
    }

    public async Task<bool> ValidateAsync(ProviderConfig config, CancellationToken cancellationToken)
    {
        var client = CreateClient(config);
        var endpoint = GetUserEndpoint(config);
        var response = await client.GetAsync(endpoint, cancellationToken);
        return response.IsSuccessStatusCode;
    }

    public async Task<IReadOnlyList<RemoteRepository>> FetchRepositoriesAsync(ProviderConfig config, CancellationToken cancellationToken)
    {
        return config.Provider switch
        {
            ProviderType.GitHub => await FetchGitHubReposAsync(config, cancellationToken),
            ProviderType.GitLab => await FetchGitLabReposAsync(config, cancellationToken),
            ProviderType.Forgejo => await FetchForgejoReposAsync(config, cancellationToken),
            _ => Array.Empty<RemoteRepository>()
        };
    }

    private HttpClient CreateClient(ProviderConfig config)
    {
        var client = _httpClientFactory.CreateClient();
        client.DefaultRequestHeaders.Accept.Clear();
        client.DefaultRequestHeaders.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

        switch (config.Provider)
        {
            case ProviderType.GitHub:
                client.DefaultRequestHeaders.UserAgent.Add(new ProductInfoHeaderValue("GitProtect", "1.0"));
                client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", config.ApiToken);
                break;
            case ProviderType.GitLab:
                client.DefaultRequestHeaders.Add("PRIVATE-TOKEN", config.ApiToken);
                break;
            case ProviderType.Forgejo:
                client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("token", config.ApiToken);
                break;
        }

        return client;
    }

    private string GetUserEndpoint(ProviderConfig config)
    {
        return config.Provider switch
        {
            ProviderType.GitHub => BuildGitHubApiBase(config) + "/user",
            ProviderType.GitLab => BuildGitLabApiBase(config) + "/user",
            ProviderType.Forgejo => BuildForgejoApiBase(config) + "/user",
            _ => string.Empty
        };
    }

    private async Task<IReadOnlyList<RemoteRepository>> FetchGitHubReposAsync(ProviderConfig config, CancellationToken cancellationToken)
    {
        var client = CreateClient(config);
        var apiBase = BuildGitHubApiBase(config);
        var repos = new List<RemoteRepository>();
        var page = 1;

        while (true)
        {
            var url = $"{apiBase}/user/repos?per_page=100&page={page}&affiliation=owner,collaborator,organization_member";
            var response = await client.GetAsync(url, cancellationToken);
            response.EnsureSuccessStatusCode();

            var items = await response.Content.ReadFromJsonAsync<List<GitHubRepo>>(cancellationToken: cancellationToken) ?? new();
            repos.AddRange(items.Select(r => new RemoteRepository(r.id.ToString(), r.name, r.full_name, r.clone_url, r.default_branch)));

            if (items.Count < 100)
            {
                break;
            }

            page++;
        }

        return repos;
    }

    private async Task<IReadOnlyList<RemoteRepository>> FetchGitLabReposAsync(ProviderConfig config, CancellationToken cancellationToken)
    {
        var client = CreateClient(config);
        var apiBase = BuildGitLabApiBase(config);
        var repos = new List<RemoteRepository>();
        var page = 1;

        while (true)
        {
            var url = $"{apiBase}/projects?membership=true&per_page=100&page={page}";
            var response = await client.GetAsync(url, cancellationToken);
            response.EnsureSuccessStatusCode();

            var items = await response.Content.ReadFromJsonAsync<List<GitLabRepo>>(cancellationToken: cancellationToken) ?? new();
            repos.AddRange(items.Select(r => new RemoteRepository(r.id.ToString(), r.name, r.path_with_namespace, r.http_url_to_repo, r.default_branch)));

            if (items.Count < 100)
            {
                break;
            }

            page++;
        }

        return repos;
    }

    private async Task<IReadOnlyList<RemoteRepository>> FetchForgejoReposAsync(ProviderConfig config, CancellationToken cancellationToken)
    {
        var client = CreateClient(config);
        var apiBase = BuildForgejoApiBase(config);
        var repos = new List<RemoteRepository>();
        var page = 1;

        while (true)
        {
            var url = $"{apiBase}/user/repos?limit=50&page={page}";
            var response = await client.GetAsync(url, cancellationToken);
            response.EnsureSuccessStatusCode();

            var items = await response.Content.ReadFromJsonAsync<List<ForgejoRepo>>(cancellationToken: cancellationToken) ?? new();
            repos.AddRange(items.Select(r => new RemoteRepository(r.id.ToString(), r.name, r.full_name, r.clone_url, r.default_branch)));

            if (items.Count < 50)
            {
                break;
            }

            page++;
        }

        return repos;
    }

    private static string BuildGitHubApiBase(ProviderConfig config)
    {
        var baseUrl = config.BaseUrl.TrimEnd('/');
        if (baseUrl.Contains("api.github.com", StringComparison.OrdinalIgnoreCase))
        {
            return baseUrl;
        }

        if (baseUrl.Contains("github.com", StringComparison.OrdinalIgnoreCase))
        {
            return "https://api.github.com";
        }

        if (baseUrl.EndsWith("/api/v3", StringComparison.OrdinalIgnoreCase))
        {
            return baseUrl;
        }

        return baseUrl + "/api/v3";
    }

    private static string BuildGitLabApiBase(ProviderConfig config)
    {
        var baseUrl = config.BaseUrl.TrimEnd('/');
        if (baseUrl.EndsWith("/api/v4", StringComparison.OrdinalIgnoreCase))
        {
            return baseUrl;
        }

        return baseUrl + "/api/v4";
    }

    private static string BuildForgejoApiBase(ProviderConfig config)
    {
        var baseUrl = config.BaseUrl.TrimEnd('/');
        if (baseUrl.EndsWith("/api/v1", StringComparison.OrdinalIgnoreCase))
        {
            return baseUrl;
        }

        return baseUrl + "/api/v1";
    }

    private sealed record GitHubRepo(long id, string name, string full_name, string clone_url, string? default_branch);
    private sealed record GitLabRepo(int id, string name, string path_with_namespace, string http_url_to_repo, string? default_branch);
    private sealed record ForgejoRepo(long id, string name, string full_name, string clone_url, string? default_branch);
}

public sealed record RemoteRepository(string ExternalId, string Name, string FullName, string CloneUrl, string? DefaultBranch);
