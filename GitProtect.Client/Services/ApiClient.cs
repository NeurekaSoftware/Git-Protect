using System.Net.Http.Json;
using GitProtect.Client.Models;
using Microsoft.AspNetCore.Components;
using System.Text.Json;

namespace GitProtect.Client.Services;

public sealed record ApiResult<T>(bool Success, T? Data, string? Error);

public sealed class ApiClient
{
    private readonly HttpClient _http;

    public ApiClient(HttpClient http, NavigationManager navigationManager)
    {
        _http = http;
        if (_http.BaseAddress is null)
        {
            _http.BaseAddress = new Uri(navigationManager.BaseUri);
        }
    }

    public async Task<SetupStatusDto?> GetStatusAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<SetupStatusDto>("api/status", cancellationToken);

    public async Task<List<ProviderStatusDto>> GetProvidersAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<List<ProviderStatusDto>>("api/providers", cancellationToken) ?? new();

    public async Task<ApiResult<ProviderStatusDto>> SaveProviderAsync(ProviderType provider, ProviderUpsertRequest request, CancellationToken cancellationToken = default)
    {
        var response = await _http.PutAsJsonAsync($"api/providers/{provider}", request, cancellationToken);
        return await ReadResponseAsync<ProviderStatusDto>(response, cancellationToken);
    }

    public async Task<bool> SyncReposAsync(ProviderType provider, CancellationToken cancellationToken = default)
    {
        var response = await _http.PostAsync($"api/providers/{provider}/sync", null, cancellationToken);
        return response.IsSuccessStatusCode;
    }

    public async Task<List<RepositoryDto>> GetReposAsync(ProviderType provider, CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<List<RepositoryDto>>($"api/providers/{provider}/repos", cancellationToken) ?? new();

    public async Task<BackupTaskDto?> RunBackupAsync(ProviderType provider, CancellationToken cancellationToken = default)
    {
        var response = await _http.PostAsync($"api/providers/{provider}/backup", null, cancellationToken);
        if (!response.IsSuccessStatusCode)
        {
            return null;
        }
        return await response.Content.ReadFromJsonAsync<BackupTaskDto>(cancellationToken: cancellationToken);
    }

    public async Task<BackupTaskDto?> RunRepositoryBackupAsync(ProviderType provider, int repoId, CancellationToken cancellationToken = default)
    {
        var response = await _http.PostAsync($"api/providers/{provider}/backup/{repoId}", null, cancellationToken);
        if (!response.IsSuccessStatusCode)
        {
            return null;
        }
        return await response.Content.ReadFromJsonAsync<BackupTaskDto>(cancellationToken: cancellationToken);
    }

    public async Task<List<BackupTaskDto>> GetTasksAsync(int? limit = null, CancellationToken cancellationToken = default)
    {
        var url = limit.HasValue ? $"api/tasks?limit={limit.Value}" : "api/tasks";
        return await _http.GetFromJsonAsync<List<BackupTaskDto>>(url, cancellationToken) ?? new();
    }

    public async Task<StorageDetailsDto?> GetStorageAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<StorageDetailsDto>("api/storage", cancellationToken);

    public async Task<ApiResult<S3ConfigDto>> SaveStorageAsync(S3UpsertRequest request, CancellationToken cancellationToken = default)
    {
        var response = await _http.PutAsJsonAsync("api/storage", request, cancellationToken);
        return await ReadResponseAsync<S3ConfigDto>(response, cancellationToken);
    }

    public async Task<BackupScheduleDto?> GetBackupScheduleAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<BackupScheduleDto>("api/schedule", cancellationToken);

    public async Task<ApiResult<BackupScheduleDto>> SaveBackupScheduleAsync(BackupScheduleUpsertRequest request, CancellationToken cancellationToken = default)
    {
        var response = await _http.PutAsJsonAsync("api/schedule", request, cancellationToken);
        return await ReadResponseAsync<BackupScheduleDto>(response, cancellationToken);
    }

    public async Task<RetentionPolicyDto?> GetRetentionPolicyAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<RetentionPolicyDto>("api/retention", cancellationToken);

    public async Task<ApiResult<RetentionPolicyDto>> SaveRetentionPolicyAsync(RetentionPolicyUpsertRequest request, CancellationToken cancellationToken = default)
    {
        var response = await _http.PutAsJsonAsync("api/retention", request, cancellationToken);
        return await ReadResponseAsync<RetentionPolicyDto>(response, cancellationToken);
    }

    public async Task<ApiResult<RetentionPruneResultDto>> PruneRetentionAsync(CancellationToken cancellationToken = default)
    {
        var response = await _http.PostAsync("api/retention/prune", null, cancellationToken);
        return await ReadResponseAsync<RetentionPruneResultDto>(response, cancellationToken);
    }

    public async Task<DashboardDto?> GetDashboardAsync(CancellationToken cancellationToken = default)
        => await _http.GetFromJsonAsync<DashboardDto>("api/dashboard", cancellationToken);

    private static async Task<ApiResult<T>> ReadResponseAsync<T>(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (response.IsSuccessStatusCode)
        {
            var data = await response.Content.ReadFromJsonAsync<T>(cancellationToken: cancellationToken);
            return new ApiResult<T>(true, data, null);
        }

        var error = await TryReadErrorMessageAsync(response, cancellationToken);
        return new ApiResult<T>(false, default, error ?? response.ReasonPhrase);
    }

    private static async Task<string?> TryReadErrorMessageAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        try
        {
            var body = await response.Content.ReadAsStringAsync(cancellationToken);
            if (string.IsNullOrWhiteSpace(body))
            {
                return null;
            }

            using var doc = JsonDocument.Parse(body);
            if (doc.RootElement.TryGetProperty("message", out var message))
            {
                return message.GetString();
            }

            return body;
        }
        catch
        {
            return null;
        }
    }
}
