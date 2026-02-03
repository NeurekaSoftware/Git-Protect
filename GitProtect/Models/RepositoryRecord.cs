namespace GitProtect.Models;

public sealed class RepositoryRecord
{
    public int Id { get; set; }
    public ProviderType Provider { get; set; }
    public string ExternalId { get; set; } = string.Empty;
    public string Name { get; set; } = string.Empty;
    public string FullName { get; set; } = string.Empty;
    public string CloneUrl { get; set; } = string.Empty;
    public string? DefaultBranch { get; set; }
    public BackupStatus LastBackupStatus { get; set; } = BackupStatus.Never;
    public DateTimeOffset? LastBackupAt { get; set; }
    public string? LastBackupMessage { get; set; }
    public long? LastBackupSizeBytes { get; set; }
}
