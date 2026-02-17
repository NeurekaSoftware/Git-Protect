using System.Formats.Tar;
using System.IO.Compression;
using System.Reflection;
using System.Buffers.Binary;
using System.Security.Cryptography;
using System.Text;
using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Paths;
using Genbox.SimpleS3.Core.Abstracts.Clients;
using Genbox.SimpleS3.Core.Abstracts.Enums;
using Genbox.SimpleS3.Core.Abstracts.Response;
using Genbox.SimpleS3.Core.Common.Authentication;
using Genbox.SimpleS3.Core.Common.Exceptions;
using Genbox.SimpleS3.Core.Extensions;
using Genbox.SimpleS3.Core.Network.Responses.Objects;
using Genbox.SimpleS3.Extensions.GenericS3;
using Genbox.SimpleS3.GenericS3;
using Genbox.SimpleS3.ProviderBase;

namespace CLI.Services.Storage;

public sealed class SimpleS3ObjectStorageService : IObjectStorageService
{
    private const string ArchiveHashMetadataKey = "gitprotect-sha256";

    private readonly IObjectClient _objectClient;
    private readonly string _bucket;

    public SimpleS3ObjectStorageService(StorageConfig storage)
    {
        _bucket = storage.Bucket!;

        var requestedPathStyle = storage.ForcePathStyle == true;
        var payloadSignatureMode = ResolvePayloadSignatureMode(storage.PayloadSignatureMode);
        var alwaysCalculateContentMd5 = storage.AlwaysCalculateContentMd5 == true;
        var resolvedEndpoint = ResolveEndpoint(storage.Endpoint!, _bucket, requestedPathStyle);

        var config = new GenericS3Config
        {
            Credentials = new StringAccessKey(storage.AccessKeyId!, storage.SecretAccessKey!),
            Endpoint = resolvedEndpoint,
            RegionCode = storage.Region!,
            NamingMode = requestedPathStyle ? NamingMode.PathStyle : NamingMode.VirtualHost,
            PayloadSignatureMode = payloadSignatureMode,
            AlwaysCalculateContentMd5 = alwaysCalculateContentMd5,
            ThrowExceptionOnError = true
        };

        var client = new GenericS3Client(config, new NetworkConfig());

        _objectClient = client;
        AppLogger.Info("Object storage client initialized. provider=GenericS3");
        AppLogger.Debug(
            "Object storage settings: endpoint={Endpoint}, resolvedEndpoint={ResolvedEndpoint}, region={Region}, bucket={Bucket}, forcePathStyle={ForcePathStyle}, payloadSignatureMode={PayloadSignatureMode}, alwaysCalculateContentMd5={AlwaysCalculateContentMd5}, archiveHashMetadataKey={ArchiveHashMetadataKey}.",
            storage.Endpoint,
            resolvedEndpoint,
            storage.Region,
            storage.Bucket,
            storage.ForcePathStyle,
            payloadSignatureMode,
            alwaysCalculateContentMd5,
            ArchiveHashMetadataKey);
    }

    public async Task<ArchiveUploadResult> UploadDirectoryAsTarGzAsync(
        string localDirectory,
        string objectKey,
        CancellationToken cancellationToken,
        string? skipUploadIfSha256Matches = null,
        bool useHeadHashCheck = true)
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

        AppLogger.Debug(
            "Preparing archive upload. localDirectory={LocalDirectory}, objectKey={ObjectKey}, useHeadHashCheck={UseHeadHashCheck}.",
            localDirectory,
            normalizedObjectKey,
            useHeadHashCheck);
        string? temporaryArchivePath = null;

        try
        {
            cancellationToken.ThrowIfCancellationRequested();
            var archiveSha256 = ComputeDirectoryContentSha256(localDirectory, cancellationToken);

            if (!string.IsNullOrWhiteSpace(skipUploadIfSha256Matches) &&
                string.Equals(skipUploadIfSha256Matches, archiveSha256, StringComparison.OrdinalIgnoreCase))
            {
                AppLogger.Info(
                    "Archive upload skipped because local hash matches the latest indexed snapshot. objectKey={ObjectKey}, sha256={Sha256}.",
                    normalizedObjectKey,
                    archiveSha256);
                return new ArchiveUploadResult
                {
                    ObjectKey = normalizedObjectKey,
                    Sha256 = archiveSha256,
                    Uploaded = false,
                    ComparedWithHead = false
                };
            }

            if (useHeadHashCheck)
            {
                var headResponse = await TryHeadObjectAsync(normalizedObjectKey, cancellationToken);
                var remoteHash = TryGetMetadataValue(headResponse?.Metadata, ArchiveHashMetadataKey);

                if (headResponse is not null &&
                    !string.IsNullOrWhiteSpace(remoteHash) &&
                    string.Equals(remoteHash, archiveSha256, StringComparison.OrdinalIgnoreCase))
                {
                    AppLogger.Info(
                        "Archive upload skipped because remote metadata hash matches local hash. objectKey={ObjectKey}, sha256={Sha256}.",
                        normalizedObjectKey,
                        archiveSha256);
                    return new ArchiveUploadResult
                    {
                        ObjectKey = normalizedObjectKey,
                        Sha256 = archiveSha256,
                        Uploaded = false,
                        ComparedWithHead = true
                    };
                }
            }

            temporaryArchivePath = Path.Combine(Path.GetTempPath(), $"git-protect-{Guid.NewGuid():N}.tar.gz");
            AppLogger.Debug(
                "Creating temporary archive before upload. localDirectory={LocalDirectory}, temporaryArchivePath={TemporaryArchivePath}.",
                localDirectory,
                temporaryArchivePath);
            await using (var archiveFileStream = File.Create(temporaryArchivePath))
            await using (var gzipStream = new GZipStream(archiveFileStream, CompressionLevel.SmallestSize))
            {
                TarFile.CreateFromDirectory(localDirectory, gzipStream, includeBaseDirectory: false);
            }

            await using var stream = File.OpenRead(temporaryArchivePath);
            var response = await _objectClient.PutObjectAsync(
                _bucket,
                normalizedObjectKey,
                stream,
                request => request.Metadata.Add(ArchiveHashMetadataKey, archiveSha256),
                cancellationToken);
            EnsureSuccess(response, $"upload archive object '{normalizedObjectKey}'");
            AppLogger.Info(
                "Archive uploaded. objectKey={ObjectKey}, sha256={Sha256}, comparedWithHead={ComparedWithHead}.",
                normalizedObjectKey,
                archiveSha256,
                useHeadHashCheck);

            return new ArchiveUploadResult
            {
                ObjectKey = normalizedObjectKey,
                Sha256 = archiveSha256,
                Uploaded = true,
                ComparedWithHead = useHeadHashCheck
            };
        }
        finally
        {
            if (!string.IsNullOrWhiteSpace(temporaryArchivePath))
            {
                TryDeleteTemporaryFile(temporaryArchivePath);
            }
        }
    }

    public async Task UploadTextAsync(string objectKey, string content, CancellationToken cancellationToken)
    {
        var normalizedObjectKey = objectKey.Trim('/');
        AppLogger.Debug("Uploading text object. objectKey={ObjectKey}, bytes={ByteCount}.", normalizedObjectKey, Encoding.UTF8.GetByteCount(content));
        var bytes = Encoding.UTF8.GetBytes(content);
        await using var stream = new MemoryStream(bytes, writable: false);
        var response = await _objectClient.PutObjectAsync(_bucket, normalizedObjectKey, stream, _ => { }, cancellationToken);
        EnsureSuccess(response, $"upload text object '{normalizedObjectKey}'");
    }

    public async Task<string?> GetTextIfExistsAsync(string objectKey, CancellationToken cancellationToken)
    {
        var normalizedObjectKey = objectKey.Trim('/');
        if (string.IsNullOrWhiteSpace(normalizedObjectKey))
        {
            throw new ArgumentException("Object key is required.", nameof(objectKey));
        }

        try
        {
            using var response = await _objectClient.GetObjectAsync(_bucket, normalizedObjectKey, _ => { }, cancellationToken);
            EnsureSuccess(response, $"download text object '{normalizedObjectKey}'");

            using var reader = new StreamReader(response.Content, Encoding.UTF8);
            return await reader.ReadToEndAsync(cancellationToken);
        }
        catch (S3RequestException exception) when (IsNotFound(exception.Response))
        {
            return null;
        }
    }

    public async Task<IReadOnlyList<string>> ListObjectKeysAsync(string prefix, CancellationToken cancellationToken)
    {
        var normalizedPrefix = StorageKeyBuilder.EnsurePrefix(prefix);
        AppLogger.Debug("Listing object keys. prefix={Prefix}", normalizedPrefix);
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

        AppLogger.Info("Object key listing completed. prefix={Prefix}, keyCount={KeyCount}.", normalizedPrefix, keys.Count);
        return keys;
    }

    public async Task DeletePrefixAsync(string prefix, CancellationToken cancellationToken)
    {
        AppLogger.Info("Deleting objects under prefix={Prefix}.", prefix);
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
            AppLogger.Debug("No object deletions needed.");
            return;
        }

        AppLogger.Info("Deleting objects. count={ObjectCount}.", keys.Length);
        foreach (var batch in keys.Chunk(1000))
        {
            AppLogger.Debug("Deleting object batch. batchSize={BatchSize}.", batch.Length);
            await ObjectClientExtensions.DeleteObjectsAsync(_objectClient, _bucket, batch, _ => { }, cancellationToken);
        }
    }

    private async Task<HeadObjectResponse?> TryHeadObjectAsync(string objectKey, CancellationToken cancellationToken)
    {
        try
        {
            var response = await _objectClient.HeadObjectAsync(_bucket, objectKey, _ => { }, cancellationToken);
            EnsureSuccess(response, $"head object '{objectKey}'");
            return response;
        }
        catch (S3RequestException exception) when (IsNotFound(exception.Response))
        {
            return null;
        }
    }

    private static bool IsNotFound(IResponse? response)
    {
        return response?.StatusCode == 404;
    }

    private static string? TryGetMetadataValue(IDictionary<string, string>? metadata, string key)
    {
        if (metadata is null || metadata.Count == 0)
        {
            return null;
        }

        if (metadata.TryGetValue(key, out var exactMatch))
        {
            return exactMatch;
        }

        foreach (var item in metadata)
        {
            if (string.Equals(item.Key, key, StringComparison.OrdinalIgnoreCase))
            {
                return item.Value;
            }
        }

        return null;
    }

    private static string ComputeDirectoryContentSha256(string directoryPath, CancellationToken cancellationToken)
    {
        using var incrementalHash = IncrementalHash.CreateHash(HashAlgorithmName.SHA256);

        var entries = Directory.EnumerateFileSystemEntries(directoryPath, "*", SearchOption.AllDirectories)
            .Select(fullPath => new
            {
                FullPath = fullPath,
                RelativePath = Path.GetRelativePath(directoryPath, fullPath)
                    .Replace(Path.DirectorySeparatorChar, '/')
                    .Replace(Path.AltDirectorySeparatorChar, '/')
            })
            .OrderBy(entry => entry.RelativePath, StringComparer.Ordinal);

        foreach (var entry in entries)
        {
            cancellationToken.ThrowIfCancellationRequested();

            var attributes = File.GetAttributes(entry.FullPath);
            if ((attributes & FileAttributes.Directory) != 0)
            {
                AppendByte(incrementalHash, (byte)'D');
                AppendFramedString(incrementalHash, entry.RelativePath);
                continue;
            }

            AppendByte(incrementalHash, (byte)'F');
            AppendFramedString(incrementalHash, entry.RelativePath);

            var fileInfo = new FileInfo(entry.FullPath);
            AppendInt64(incrementalHash, fileInfo.Length);

            using var stream = File.OpenRead(entry.FullPath);
            var buffer = new byte[81920];
            while (true)
            {
                var bytesRead = stream.Read(buffer, 0, buffer.Length);
                if (bytesRead == 0)
                {
                    break;
                }

                cancellationToken.ThrowIfCancellationRequested();
                incrementalHash.AppendData(buffer.AsSpan(0, bytesRead));
            }
        }

        return Convert.ToHexString(incrementalHash.GetHashAndReset()).ToLowerInvariant();
    }

    private static void AppendFramedString(IncrementalHash hash, string value)
    {
        var bytes = Encoding.UTF8.GetBytes(value);
        AppendInt32(hash, bytes.Length);
        hash.AppendData(bytes);
    }

    private static void AppendByte(IncrementalHash hash, byte value)
    {
        Span<byte> buffer = stackalloc byte[1];
        buffer[0] = value;
        hash.AppendData(buffer);
    }

    private static void AppendInt32(IncrementalHash hash, int value)
    {
        Span<byte> buffer = stackalloc byte[sizeof(int)];
        BinaryPrimitives.WriteInt32LittleEndian(buffer, value);
        hash.AppendData(buffer);
    }

    private static void AppendInt64(IncrementalHash hash, long value)
    {
        Span<byte> buffer = stackalloc byte[sizeof(long)];
        BinaryPrimitives.WriteInt64LittleEndian(buffer, value);
        hash.AppendData(buffer);
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

    private static SignatureMode ResolvePayloadSignatureMode(string? configuredMode)
    {
        return configuredMode?.Trim().ToLowerInvariant() switch
        {
            "streaming" => SignatureMode.StreamingSignature,
            "unsigned" => SignatureMode.Unsigned,
            _ => SignatureMode.FullSignature
        };
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
            AppLogger.Warn(
                "Failed to remove temporary archive. path={Path}, error={ErrorMessage}",
                path,
                exception.Message);
        }
    }
}
