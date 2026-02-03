using System.Diagnostics;
using GitProtect.Models;
using Microsoft.Extensions.Logging;

namespace GitProtect.Services;

public sealed class GitMirrorService
{
    private readonly ILogger<GitMirrorService> _logger;
    private readonly string _mirrorRoot;

    public GitMirrorService(ILogger<GitMirrorService> logger, IConfiguration configuration)
    {
        _logger = logger;
        _mirrorRoot = configuration["Storage:MirrorRoot"] ?? "App_Data/Mirrors";
    }

    public async Task<MirrorResult> CreateOrUpdateMirrorAsync(ProviderConfig provider, RepositoryRecord repo, CancellationToken cancellationToken)
    {
        var providerFolder = provider.Provider.ToString().ToLowerInvariant();
        var safeName = repo.FullName.Replace('/', '_');
        var mirrorPath = Path.Combine(_mirrorRoot, providerFolder, $"{safeName}.git");

        Directory.CreateDirectory(Path.GetDirectoryName(mirrorPath)!);

        var cloneUrl = BuildAuthenticatedCloneUrl(repo.CloneUrl, provider.Username, provider.ApiToken);

        if (!Directory.Exists(mirrorPath))
        {
            await RunGitAsync($"clone --mirror \"{cloneUrl}\" \"{mirrorPath}\"", cancellationToken);
        }
        else
        {
            await RunGitAsync($"-C \"{mirrorPath}\" remote set-url origin \"{cloneUrl}\"", cancellationToken);
            await RunGitAsync($"-C \"{mirrorPath}\" fetch --prune --tags", cancellationToken);
        }

        await RunGitAsync($"-C \"{mirrorPath}\" lfs fetch --all", cancellationToken, allowFailure: true);

        var sizeBytes = GetDirectorySize(mirrorPath);
        return new MirrorResult(mirrorPath, sizeBytes);
    }

    private static string BuildAuthenticatedCloneUrl(string cloneUrl, string username, string token)
    {
        if (string.IsNullOrWhiteSpace(token))
        {
            return cloneUrl;
        }

        var uri = new Uri(cloneUrl);
        var user = string.IsNullOrWhiteSpace(username) ? "token" : username;
        var userInfo = $"{Uri.EscapeDataString(user)}:{Uri.EscapeDataString(token)}";
        return $"{uri.Scheme}://{userInfo}@{uri.Host}{uri.PathAndQuery}";
    }

    private async Task RunGitAsync(string arguments, CancellationToken cancellationToken, bool allowFailure = false)
    {
        var startInfo = new ProcessStartInfo
        {
            FileName = "git",
            Arguments = arguments,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
            CreateNoWindow = true
        };
        startInfo.Environment["GIT_TERMINAL_PROMPT"] = "0";

        using var process = Process.Start(startInfo);
        if (process == null)
        {
            throw new InvalidOperationException("Failed to start git process.");
        }

        var output = await process.StandardOutput.ReadToEndAsync(cancellationToken);
        var error = await process.StandardError.ReadToEndAsync(cancellationToken);
        await process.WaitForExitAsync(cancellationToken);

        if (process.ExitCode != 0)
        {
            _logger.LogWarning("Git command failed: {Arguments}. Error: {Error}", arguments, error);
            if (!allowFailure)
            {
                throw new InvalidOperationException($"Git command failed: {error}");
            }
        }
        else if (!string.IsNullOrWhiteSpace(output))
        {
            _logger.LogInformation("Git output: {Output}", output);
        }
    }

    private static long GetDirectorySize(string directory)
    {
        long size = 0;
        foreach (var file in Directory.EnumerateFiles(directory, "*", SearchOption.AllDirectories))
        {
            size += new FileInfo(file).Length;
        }
        return size;
    }
}

public sealed record MirrorResult(string Path, long SizeBytes);
