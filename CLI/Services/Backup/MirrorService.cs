using System.Security.Cryptography;
using System.Text;
using CLI.Configuration;
using CLI.Services.Git;
using CLI.Services.Paths;
using CLI.Services.Storage;

namespace CLI.Services.Backup;

public sealed class MirrorService
{
    private const string MirrorMarkerName = ".mirror-root";

    private readonly IGitRepositoryService _gitRepositoryService;
    private readonly IObjectStorageService _objectStorageService;
    private readonly string _workingRoot;

    public MirrorService(IGitRepositoryService gitRepositoryService, IObjectStorageService objectStorageService, string workingRoot)
    {
        _gitRepositoryService = gitRepositoryService;
        _objectStorageService = objectStorageService;
        _workingRoot = workingRoot;
    }

    public async Task RunAsync(Settings settings, CancellationToken cancellationToken)
    {
        var activeMirrorPrefixes = new HashSet<string>(StringComparer.Ordinal);

        foreach (var mirror in settings.Mirrors.Where(mirror => mirror?.Enabled != false))
        {
            if (mirror is null || string.IsNullOrWhiteSpace(mirror.Url))
            {
                continue;
            }

            try
            {
                var pathInfo = RepositoryPathParser.Parse(mirror.Url);
                var mirrorPrefix = StorageKeyBuilder.BuildMirrorPrefix(pathInfo);
                activeMirrorPrefixes.Add(mirrorPrefix);

                var localPath = Path.Combine(_workingRoot, "mirrors", ComputeDeterministicFolderName(mirrorPrefix));
                var credential = string.IsNullOrWhiteSpace(mirror.Credential)
                    ? null
                    : CredentialResolver.ResolveGitCredential(settings.Credentials[mirror.Credential]);

                await _gitRepositoryService.SyncBareRepositoryAsync(
                    mirror.Url,
                    localPath,
                    credential,
                    mirror.Force == true,
                    mirror.Lfs == true,
                    cancellationToken);

                await _objectStorageService.UploadDirectoryAsync(localPath, mirrorPrefix, cancellationToken);
                await _objectStorageService.UploadTextAsync($"{mirrorPrefix}/{MirrorMarkerName}", mirror.Url, cancellationToken);

                Console.WriteLine($"Mirrored '{mirror.Url}' to '{mirrorPrefix}'.");
            }
            catch (Exception exception)
            {
                Console.Error.WriteLine($"Mirror failed for '{mirror.Url}': {exception.Message}");
            }
        }

        if (settings.Storage.PruneOrphanedMirrors == true)
        {
            await PruneOrphanedMirrorsAsync(activeMirrorPrefixes, cancellationToken);
        }
    }

    private async Task PruneOrphanedMirrorsAsync(HashSet<string> activeMirrorPrefixes, CancellationToken cancellationToken)
    {
        var mirrorKeys = await _objectStorageService.ListObjectKeysAsync("mirrors/", cancellationToken);
        var existingMirrorRoots = mirrorKeys
            .Where(key => key.EndsWith($"/{MirrorMarkerName}", StringComparison.Ordinal))
            .Select(key => key[..^($"/{MirrorMarkerName}".Length)])
            .Distinct(StringComparer.Ordinal)
            .ToArray();

        foreach (var mirrorRoot in existingMirrorRoots)
        {
            if (activeMirrorPrefixes.Contains(mirrorRoot))
            {
                continue;
            }

            await _objectStorageService.DeletePrefixAsync(mirrorRoot, cancellationToken);
            Console.WriteLine($"Pruned orphaned mirror '{mirrorRoot}'.");
        }
    }

    private static string ComputeDeterministicFolderName(string value)
    {
        var bytes = SHA256.HashData(Encoding.UTF8.GetBytes(value));
        return Convert.ToHexString(bytes).ToLowerInvariant();
    }
}
