using System.Reflection;
using System.Text;
using CLI.Configuration.Models;
using CLI.Services.Paths;
using Genbox.SimpleS3.Core.Abstracts.Clients;
using Genbox.SimpleS3.Core.Extensions;
using Genbox.SimpleS3.GenericS3;
using Genbox.SimpleS3.ProviderBase;

namespace CLI.Services.Storage;

public sealed class SimpleS3ObjectStorageService : IObjectStorageService
{
    private readonly IObjectClient _objectClient;
    private readonly string _bucket;

    public SimpleS3ObjectStorageService(StorageConfig storage)
    {
        _bucket = storage.Bucket!;

        var endpoint = storage.Endpoint!.TrimEnd('/');
        if (storage.ForcePathStyle == true && !endpoint.Contains("{Bucket}", StringComparison.Ordinal))
        {
            endpoint = $"{endpoint}/{{Bucket}}";
        }

        var client = new GenericS3Client(
            storage.AccessKeyId!,
            storage.SecretAccessKey!,
            endpoint,
            storage.Region!,
            new NetworkConfig());

        _objectClient = client;
    }

    public async Task UploadDirectoryAsync(string localDirectory, string prefix, CancellationToken cancellationToken)
    {
        if (!Directory.Exists(localDirectory))
        {
            throw new DirectoryNotFoundException($"Directory '{localDirectory}' does not exist.");
        }

        var normalizedPrefix = StorageKeyBuilder.EnsurePrefix(prefix);

        foreach (var file in Directory.EnumerateFiles(localDirectory, "*", SearchOption.AllDirectories))
        {
            cancellationToken.ThrowIfCancellationRequested();

            var relativePath = Path.GetRelativePath(localDirectory, file).Replace('\\', '/');
            var objectKey = $"{normalizedPrefix}{relativePath}";

            await using var stream = File.OpenRead(file);
            await _objectClient.PutObjectAsync(_bucket, objectKey, stream, _ => { }, cancellationToken);
        }
    }

    public async Task UploadTextAsync(string objectKey, string content, CancellationToken cancellationToken)
    {
        var bytes = Encoding.UTF8.GetBytes(content);
        await using var stream = new MemoryStream(bytes, writable: false);
        await _objectClient.PutObjectAsync(_bucket, objectKey.Trim('/'), stream, _ => { }, cancellationToken);
    }

    public async Task<IReadOnlyList<string>> ListObjectKeysAsync(string prefix, CancellationToken cancellationToken)
    {
        var normalizedPrefix = StorageKeyBuilder.EnsurePrefix(prefix);
        var keys = new List<string>();

        await foreach (var item in ObjectClientExtensions
                           .ListAllObjectsAsync(_objectClient, _bucket, normalizedPrefix, false, cancellationToken)
                           .WithCancellation(cancellationToken))
        {
            var key = ExtractObjectKey(item);
            if (!string.IsNullOrWhiteSpace(key))
            {
                keys.Add(key);
            }
        }

        return keys;
    }

    public async Task DeletePrefixAsync(string prefix, CancellationToken cancellationToken)
    {
        var objectKeys = await ListObjectKeysAsync(prefix, cancellationToken);
        await DeleteObjectsAsync(objectKeys, cancellationToken);
    }

    public async Task DeleteObjectsAsync(IEnumerable<string> objectKeys, CancellationToken cancellationToken)
    {
        var keys = objectKeys
            .Where(key => !string.IsNullOrWhiteSpace(key))
            .Select(key => key.Trim('/'))
            .Distinct(StringComparer.Ordinal)
            .ToArray();

        if (keys.Length == 0)
        {
            return;
        }

        foreach (var batch in keys.Chunk(1000))
        {
            await ObjectClientExtensions.DeleteObjectsAsync(_objectClient, _bucket, batch, _ => { }, cancellationToken);
        }
    }

    private static string? ExtractObjectKey(object item)
    {
        return ReadStringProperty(item, "ObjectKey")
               ?? ReadStringProperty(item, "Key")
               ?? ReadStringProperty(item, "Name");
    }

    private static string? ReadStringProperty(object instance, string propertyName)
    {
        var property = instance.GetType().GetProperty(propertyName, BindingFlags.Public | BindingFlags.Instance);
        if (property?.PropertyType != typeof(string))
        {
            return null;
        }

        return (string?)property.GetValue(instance);
    }
}
