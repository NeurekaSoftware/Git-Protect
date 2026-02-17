namespace CLI.Services.Storage;

public interface IObjectStorageService
{
    Task<ArchiveUploadResult> UploadDirectoryAsTarGzAsync(
        string localDirectory,
        string objectKey,
        CancellationToken cancellationToken,
        string? skipUploadIfSha256Matches = null,
        bool useHeadHashCheck = true);

    Task UploadTextAsync(string objectKey, string content, CancellationToken cancellationToken);

    Task<string?> GetTextIfExistsAsync(string objectKey, CancellationToken cancellationToken);

    Task<IReadOnlyList<string>> ListObjectKeysAsync(string prefix, CancellationToken cancellationToken);

    Task DeletePrefixAsync(string prefix, CancellationToken cancellationToken);

    Task DeleteObjectsAsync(IEnumerable<string> objectKeys, CancellationToken cancellationToken);
}
