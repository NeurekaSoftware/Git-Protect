namespace GitProtect.Models;

public sealed class BackupTask
{
    public int Id { get; set; }
    public ProviderType Provider { get; set; }
    public int? RepositoryId { get; set; }
    public string Name { get; set; } = string.Empty;
    public BackupTaskStatus Status { get; set; } = BackupTaskStatus.Pending;
    public int Progress { get; set; }
    public DateTime CreatedAt { get; set; } = DateTime.UtcNow;
    public DateTime? StartedAt { get; set; }
    public DateTime? CompletedAt { get; set; }
    public string? Message { get; set; }
}
