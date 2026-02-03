namespace GitProtect.Client.Models;

public enum ProviderType
{
    GitHub = 0,
    GitLab = 1,
    Forgejo = 2
}

public enum BackupStatus
{
    Never = 0,
    Running = 1,
    Success = 2,
    Failed = 3
}

public enum BackupTaskStatus
{
    Pending = 0,
    Running = 1,
    Success = 2,
    Failed = 3
}

public enum BackupTaskType
{
    Backup = 0,
    Prune = 1
}

public enum BackupTaskTrigger
{
    Manual = 0,
    Scheduled = 1
}

public sealed record SetupStatusDto(bool StorageConfigured, bool StorageVerified, IReadOnlyList<ProviderStatusDto> Providers);

public sealed record ProviderStatusDto(
    ProviderType Provider,
    string BaseUrl,
    string Username,
    bool IsConfigured,
    bool IsVerified,
    DateTime? VerifiedAt,
    DateTime? LastSyncAt);

public sealed record ProviderUpsertRequest(string BaseUrl, string Username, string ApiToken);

public sealed record S3ConfigDto(
    string Endpoint,
    string Region,
    string Bucket,
    string AccessKeyId,
    string SecretAccessKey,
    bool UsePathStyle,
    bool IsVerified,
    DateTime? VerifiedAt);

public sealed record S3UpsertRequest(string Endpoint, string Region, string Bucket, string AccessKeyId, string SecretAccessKey, bool UsePathStyle);

public sealed record StorageDetailsDto(S3ConfigDto? Config, long TotalBytes, DateTime? LastVerifiedAt);

public sealed record RepositoryDto(
    int Id,
    string Name,
    string FullName,
    BackupStatus Status,
    DateTime? LastBackupAt,
    long? LastBackupSizeBytes,
    string? LastBackupMessage);

public sealed record BackupTaskDto(
    int Id,
    string Name,
    ProviderType? Provider,
    int? RepositoryId,
    BackupTaskStatus Status,
    BackupTaskType TaskType,
    BackupTaskTrigger Trigger,
    int Progress,
    DateTime CreatedAt,
    DateTime? StartedAt,
    DateTime? CompletedAt,
    string? Message);

public sealed record DashboardDto(
    int TotalRepositories,
    int ProtectedCount,
    int FailedCount,
    int PendingCount,
    IReadOnlyList<ChartPointDto> Activity);

public sealed record ChartPointDto(string Label, int Value);

public sealed record BackupScheduleDto(bool IsEnabled, string CronExpression);

public sealed record BackupScheduleUpsertRequest(bool IsEnabled, string CronExpression);

public sealed record RetentionPolicyDto(bool IsEnabled, int RetentionDays);

public sealed record RetentionPolicyUpsertRequest(bool IsEnabled, int RetentionDays);

public sealed record RetentionPruneResultDto(int DeletedCount, DateTime CutoffUtc);
