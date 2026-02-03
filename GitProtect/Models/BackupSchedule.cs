using GitProtect.Validation;

namespace GitProtect.Models;

public sealed class BackupSchedule
{
    public int Id { get; set; }
    public bool IsEnabled { get; set; }
    public string CronExpression { get; set; } = CronExpressionValidator.DefaultDailyExpression;
}
