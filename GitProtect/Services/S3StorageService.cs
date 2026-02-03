using Genbox.SimpleS3.Core.Common.Authentication;
using Genbox.SimpleS3.Extensions.GenericS3;
using Genbox.SimpleS3.GenericS3;
using Genbox.SimpleS3.ProviderBase;
using GitProtect.Models;

namespace GitProtect.Services;

public sealed class S3StorageService
{
    public async Task VerifyAsync(S3Config config, CancellationToken cancellationToken)
    {
        using var client = CreateClient(config);
        var response = await client.HeadBucketAsync(config.Bucket, _ => { }, cancellationToken);
        if (response.IsSuccess)
        {
            return;
        }

        if (response.StatusCode == 405)
        {
            // Some S3-compatible providers (e.g. Backblaze B2) don't support HEAD on buckets.
            var listResponse = await client.ListObjectsAsync(config.Bucket, request =>
            {
                request.MaxKeys = 1;
            }, cancellationToken);

            if (listResponse.IsSuccess)
            {
                return;
            }

            ThrowDetailedError(
                "Unable to verify bucket with provided credentials.",
                listResponse.StatusCode,
                listResponse.Error?.GetErrorDetails(),
                listResponse.Error?.Message,
                listResponse.RequestId,
                listResponse.ResponseId);
        }

        ThrowDetailedError(
            "Unable to verify bucket with provided credentials.",
            response.StatusCode,
            response.Error?.GetErrorDetails(),
            response.Error?.Message,
            response.RequestId,
            response.ResponseId);
    }

    private static void ThrowDetailedError(string summary, int statusCode, string? errorDetails, string? errorMessage, string? requestId, string? responseId)
    {
        if (string.IsNullOrWhiteSpace(errorDetails))
        {
            errorDetails = errorMessage;
        }

        var details = string.IsNullOrWhiteSpace(errorDetails)
            ? $"StatusCode={statusCode}"
            : $"StatusCode={statusCode}, Error={errorDetails}";

        if (!string.IsNullOrWhiteSpace(requestId) || !string.IsNullOrWhiteSpace(responseId))
        {
            details += $", RequestId={requestId}, ResponseId={responseId}";
        }

        throw new InvalidOperationException($"{summary} {details}");
    }

    public async Task<long> UploadDirectoryAsync(S3Config config, string sourcePath, string prefix, CancellationToken cancellationToken)
    {
        if (!Directory.Exists(sourcePath))
        {
            throw new DirectoryNotFoundException($"Mirror path not found: {sourcePath}");
        }

        using var client = CreateClient(config);
        var normalizedPrefix = prefix.TrimStart('/');
        if (!normalizedPrefix.EndsWith("/", StringComparison.Ordinal))
        {
            normalizedPrefix += "/";
        }

        long totalBytes = 0;
        foreach (var file in Directory.EnumerateFiles(sourcePath, "*", SearchOption.AllDirectories))
        {
            var relativePath = Path.GetRelativePath(sourcePath, file)
                .Replace(Path.DirectorySeparatorChar, '/');
            var key = normalizedPrefix + relativePath;

            await using var stream = File.OpenRead(file);
            var response = await client.PutObjectAsync(config.Bucket, key, stream, _ => { }, cancellationToken);
            if (!response.IsSuccess)
            {
                ThrowDetailedError(
                    $"Upload failed for {key}.",
                    response.StatusCode,
                    response.Error?.GetErrorDetails(),
                    response.Error?.Message,
                    response.RequestId,
                    response.ResponseId);
            }

            totalBytes += stream.Length;
        }

        return totalBytes;
    }

    private static GenericS3Client CreateClient(S3Config config)
    {
        var endpoint = NormalizeEndpoint(config);
        var useTls = endpoint.StartsWith("https", StringComparison.OrdinalIgnoreCase);

        var s3Config = new GenericS3Config
        {
            Endpoint = endpoint,
            RegionCode = config.Region,
            UseTls = useTls,
            Credentials = new StringAccessKey(config.AccessKeyId, config.SecretAccessKey),
            ThrowExceptionOnError = false
        };

        var networkConfig = new NetworkConfig
        {
            Timeout = TimeSpan.FromSeconds(60)
        };

        return new GenericS3Client(s3Config, networkConfig);
    }

    private static string NormalizeEndpoint(S3Config config)
    {
        var trimmed = config.Endpoint.Trim();
        if (!config.UsePathStyle)
        {
            if (trimmed.Contains("{Bucket}", StringComparison.OrdinalIgnoreCase))
            {
                return trimmed;
            }

            if (!string.IsNullOrWhiteSpace(config.Bucket))
            {
                var bucketPrefix = config.Bucket.Trim('/') + ".";

                if (Uri.TryCreate(trimmed, UriKind.Absolute, out var parsedUri))
                {
                    if (parsedUri.Host.StartsWith(bucketPrefix, StringComparison.OrdinalIgnoreCase))
                    {
                        return trimmed;
                    }

                    var virtualScheme = parsedUri.Scheme;
                    var hostPort = parsedUri.IsDefaultPort ? parsedUri.Host : $"{parsedUri.Host}:{parsedUri.Port}";
                    var path = parsedUri.AbsolutePath.TrimEnd('/');
                    var pathSegment = path == "/" ? string.Empty : path;
                    return $"{virtualScheme}://{{Bucket}}.{hostPort}{pathSegment}";
                }

                if (trimmed.StartsWith(bucketPrefix, StringComparison.OrdinalIgnoreCase))
                {
                    return $"https://{trimmed}";
                }

                return $"https://{{Bucket}}.{trimmed.TrimStart('/')}";
            }

            return trimmed;
        }

        if (!string.IsNullOrWhiteSpace(config.Bucket))
        {
            var bucketSuffix = "/" + config.Bucket.Trim('/');
            if (trimmed.EndsWith(bucketSuffix, StringComparison.OrdinalIgnoreCase))
            {
                trimmed = trimmed.Substring(0, trimmed.Length - bucketSuffix.Length);
            }
        }

        if (trimmed.Contains("{Bucket}", StringComparison.OrdinalIgnoreCase))
        {
            return trimmed;
        }

        var scheme = "https";
        var hostAndPath = trimmed;

        if (Uri.TryCreate(trimmed, UriKind.Absolute, out var uri))
        {
            scheme = uri.Scheme;
            var basePath = uri.AbsolutePath.TrimEnd('/');
            hostAndPath = uri.Authority + (basePath == "/" ? string.Empty : basePath);
        }
        else if (trimmed.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
        {
            scheme = "https";
            hostAndPath = trimmed.Substring("https://".Length);
        }
        else if (trimmed.StartsWith("http://", StringComparison.OrdinalIgnoreCase))
        {
            scheme = "http";
            hostAndPath = trimmed.Substring("http://".Length);
        }

        hostAndPath = hostAndPath.TrimEnd('/');
        return $"{scheme}://{hostAndPath}/{{Bucket}}";
    }
}
