using System.ComponentModel.DataAnnotations;
using GitProtect.Models;
using GitProtect.Validation;

namespace GitProtect.Endpoints;

public sealed record SetupStatusDto(
    bool StorageConfigured,
    bool StorageVerified,
    IReadOnlyList<ProviderStatusDto> Providers);

public sealed record ProviderStatusDto(
    ProviderType Provider,
    string BaseUrl,
    string Username,
    bool IsConfigured,
    bool IsVerified,
    DateTimeOffset? VerifiedAt,
    DateTimeOffset? LastSyncAt);

public sealed record ProviderUpsertRequest(string BaseUrl, string Username, string ApiToken);

public sealed record S3ConfigDto(
    string Endpoint,
    string Region,
    string Bucket,
    string AccessKeyId,
    string SecretAccessKey,
    bool UsePathStyle,
    bool IsVerified,
    DateTimeOffset? VerifiedAt);

public sealed record StorageDetailsDto(
    S3ConfigDto? Config,
    long TotalBytes,
    DateTimeOffset? LastVerifiedAt);

public sealed record S3UpsertRequest(string Endpoint, string Region, string Bucket, string AccessKeyId, string SecretAccessKey, bool UsePathStyle);

public sealed record BackupScheduleDto(bool IsEnabled, string CronExpression);

public sealed record BackupScheduleUpsertRequest(
    bool IsEnabled,
    [property: Required, CronExpression] string CronExpression);

public sealed record RepositoryDto(
    int Id,
    string Name,
    string FullName,
    BackupStatus Status,
    DateTimeOffset? LastBackupAt,
    long? LastBackupSizeBytes,
    string? LastBackupMessage);

public sealed record BackupTaskDto(
    int Id,
    string Name,
    ProviderType Provider,
    int? RepositoryId,
    BackupTaskStatus Status,
    int Progress,
    DateTimeOffset CreatedAt,
    DateTimeOffset? StartedAt,
    DateTimeOffset? CompletedAt,
    string? Message);

public sealed record DashboardDto(
    int TotalRepositories,
    int ProtectedCount,
    int FailedCount,
    int PendingCount,
    IReadOnlyList<ChartPointDto> Activity);

public sealed record ChartPointDto(string Label, int Value);

public sealed record StorageSummaryDto(
    string Bucket,
    string Region,
    long TotalBytes,
    DateTimeOffset? LastVerifiedAt);
