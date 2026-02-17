using System.Reflection;
using System.Formats.Tar;
using System.IO.Compression;
using System.Text;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Paths;
using Genbox.SimpleS3.Core.Abstracts.Clients;
using Genbox.SimpleS3.Core.Abstracts.Enums;
using Genbox.SimpleS3.Core.Abstracts.Response;
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
        var requestedPathStyle = storage.ForcePathStyle == true;
        var resolvedEndpoint = ResolveEndpoint(storage.Endpoint!, _bucket, requestedPathStyle);

        var config = new GenericS3Config
        {
            Credentials = new StringAccessKey(storage.AccessKeyId!, storage.SecretAccessKey!),
            Endpoint = resolvedEndpoint,
            RegionCode = storage.Region!,
            NamingMode = requestedPathStyle ? NamingMode.PathStyle : NamingMode.VirtualHost,
            ThrowExceptionOnError = true
        };

        var client = new GenericS3Client(config, new NetworkConfig());

        _objectClient = client;
        AppLogger.Info("storage: initialized S3 client.");
        AppLogger.Debug(
            $"storage: endpoint='{storage.Endpoint}', resolvedEndpoint='{resolvedEndpoint}', region='{storage.Region}', bucket='{storage.Bucket}', forcePathStyle='{storage.ForcePathStyle}'.");
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
            var response = await _objectClient.PutObjectAsync(_bucket, objectKey, stream, _ => { }, cancellationToken);
            EnsureSuccess(response, $"upload object '{objectKey}'");
            uploadedCount++;
        }

        AppLogger.Info($"storage: uploaded {uploadedCount} object(s) to prefix '{normalizedPrefix}'.");
    }

    public async Task UploadDirectoryAsTarGzAsync(string localDirectory, string objectKey, CancellationToken cancellationToken)
    {
        if (!Directory.Exists(localDirectory))
        {
            throw new DirectoryNotFoundException($"Directory '{localDirectory}' does not exist.");
        }

        var normalizedObjectKey = objectKey.Trim('/');
        if (string.IsNullOrWhiteSpace(normalizedObjectKey))
        {
            throw new ArgumentException("Object key is required.", nameof(objectKey));
        }

        var temporaryArchivePath = Path.Combine(Path.GetTempPath(), $"git-protect-{Guid.NewGuid():N}.tar.gz");
        AppLogger.Info($"storage: creating archive from '{localDirectory}' for object '{normalizedObjectKey}'.");

        try
        {
            cancellationToken.ThrowIfCancellationRequested();

            await using (var archiveFileStream = File.Create(temporaryArchivePath))
            await using (var gzipStream = new GZipStream(archiveFileStream, CompressionLevel.SmallestSize))
            {
                TarFile.CreateFromDirectory(localDirectory, gzipStream, includeBaseDirectory: false);
            }

            cancellationToken.ThrowIfCancellationRequested();

            AppLogger.Info($"storage: uploading archive '{temporaryArchivePath}' as object '{normalizedObjectKey}'.");
            await using var stream = File.OpenRead(temporaryArchivePath);
            var response = await _objectClient.PutObjectAsync(_bucket, normalizedObjectKey, stream, _ => { }, cancellationToken);
            EnsureSuccess(response, $"upload archive object '{normalizedObjectKey}'");
        }
        finally
        {
            TryDeleteTemporaryFile(temporaryArchivePath);
        }
    }

    public async Task UploadTextAsync(string objectKey, string content, CancellationToken cancellationToken)
    {
        AppLogger.Info($"storage: uploading text object '{objectKey.Trim('/')}'.");
        var bytes = Encoding.UTF8.GetBytes(content);
        await using var stream = new MemoryStream(bytes, writable: false);
        var response = await _objectClient.PutObjectAsync(_bucket, objectKey.Trim('/'), stream, _ => { }, cancellationToken);
        EnsureSuccess(response, $"upload text object '{objectKey.Trim('/')}'");
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

    private static string ResolveEndpoint(string configuredEndpoint, string bucket, bool forcePathStyle)
    {
        var endpoint = configuredEndpoint.Trim();
        if (string.IsNullOrWhiteSpace(endpoint))
        {
            return endpoint;
        }

        if (endpoint.Contains('{'))
        {
            return endpoint.TrimEnd('/');
        }

        if (!Uri.TryCreate(endpoint, UriKind.Absolute, out var uri))
        {
            return endpoint.TrimEnd('/');
        }

        if (forcePathStyle)
        {
            return endpoint.TrimEnd('/');
        }

        var host = uri.Host;
        var bucketPrefix = $"{bucket}.";
        if (host.StartsWith(bucketPrefix, StringComparison.OrdinalIgnoreCase))
        {
            host = host[bucketPrefix.Length..];
        }

        var authority = uri.IsDefaultPort ? host : $"{host}:{uri.Port}";
        var path = uri.AbsolutePath == "/" ? string.Empty : uri.AbsolutePath.TrimEnd('/');
        return $"{uri.Scheme}://{{Bucket}}.{authority}{path}";
    }

    private static void EnsureSuccess(IResponse response, string operation)
    {
        if (response.IsSuccess)
        {
            return;
        }

        var detail = response.Error is null
            ? $"statusCode={response.StatusCode}"
            : $"{response.Error.Code}: {response.Error.Message} ({response.Error.GetErrorDetails()})";

        throw new InvalidOperationException($"storage: failed to {operation}. {detail}");
    }

    private static void TryDeleteTemporaryFile(string path)
    {
        try
        {
            if (File.Exists(path))
            {
                File.Delete(path);
            }
        }
        catch (Exception exception)
        {
            AppLogger.Warn($"storage: failed to remove temporary archive '{path}': {exception.Message}");
        }
    }
}
