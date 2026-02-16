using System.Reflection;
using System.Text;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Paths;
using Genbox.SimpleS3.Core.Abstracts.Clients;
using Genbox.SimpleS3.Core.Abstracts.Enums;
using Genbox.SimpleS3.Core.Common.Authentication;
using Genbox.SimpleS3.Core.Extensions;
using Genbox.SimpleS3.Extensions.GenericS3;
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

        var config = new GenericS3Config
        {
            Credentials = new StringAccessKey(storage.AccessKeyId!, storage.SecretAccessKey!),
            Endpoint = storage.Endpoint!.TrimEnd('/'),
            RegionCode = storage.Region!,
            NamingMode = storage.ForcePathStyle == true ? NamingMode.PathStyle : NamingMode.VirtualHost
        };

        var client = new GenericS3Client(config, new NetworkConfig());

        _objectClient = client;
        AppLogger.Info("storage: initialized S3 client.");
        AppLogger.Debug(
            $"storage: endpoint='{storage.Endpoint}', region='{storage.Region}', bucket='{storage.Bucket}', forcePathStyle='{storage.ForcePathStyle}'.");
    }

    public async Task UploadDirectoryAsync(string localDirectory, string prefix, CancellationToken cancellationToken)
    {
        if (!Directory.Exists(localDirectory))
        {
            throw new DirectoryNotFoundException($"Directory '{localDirectory}' does not exist.");
        }

        var normalizedPrefix = StorageKeyBuilder.EnsurePrefix(prefix);
        AppLogger.Info($"storage: uploading directory '{localDirectory}' to prefix '{normalizedPrefix}'.");
        var uploadedCount = 0;

        foreach (var file in Directory.EnumerateFiles(localDirectory, "*", SearchOption.AllDirectories))
        {
            cancellationToken.ThrowIfCancellationRequested();

            var relativePath = Path.GetRelativePath(localDirectory, file).Replace('\\', '/');
            var objectKey = $"{normalizedPrefix}{relativePath}";
            AppLogger.Debug($"storage: uploading file '{file}' as object '{objectKey}'.");

            await using var stream = File.OpenRead(file);
            await _objectClient.PutObjectAsync(_bucket, objectKey, stream, _ => { }, cancellationToken);
            uploadedCount++;
        }

        AppLogger.Info($"storage: uploaded {uploadedCount} object(s) to prefix '{normalizedPrefix}'.");
    }

    public async Task UploadTextAsync(string objectKey, string content, CancellationToken cancellationToken)
    {
        AppLogger.Info($"storage: uploading text object '{objectKey.Trim('/')}'.");
        var bytes = Encoding.UTF8.GetBytes(content);
        await using var stream = new MemoryStream(bytes, writable: false);
        await _objectClient.PutObjectAsync(_bucket, objectKey.Trim('/'), stream, _ => { }, cancellationToken);
    }

    public async Task<IReadOnlyList<string>> ListObjectKeysAsync(string prefix, CancellationToken cancellationToken)
    {
        var normalizedPrefix = StorageKeyBuilder.EnsurePrefix(prefix);
        AppLogger.Info($"storage: listing object keys under prefix '{normalizedPrefix}'.");
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

        AppLogger.Info($"storage: listed {keys.Count} object key(s) under prefix '{normalizedPrefix}'.");
        return keys;
    }

    public async Task DeletePrefixAsync(string prefix, CancellationToken cancellationToken)
    {
        AppLogger.Info($"storage: deleting prefix '{prefix}'.");
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
            AppLogger.Info("storage: no objects to delete.");
            return;
        }

        AppLogger.Info($"storage: deleting {keys.Length} object(s).");
        foreach (var batch in keys.Chunk(1000))
        {
            AppLogger.Debug($"storage: deleting batch with {batch.Length} object(s).");
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
