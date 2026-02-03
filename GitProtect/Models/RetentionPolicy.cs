namespace GitProtect.Models;

public sealed class RetentionPolicy
{
    public int Id { get; set; }
    public bool IsEnabled { get; set; }
    public int RetentionDays { get; set; } = 30;
}
