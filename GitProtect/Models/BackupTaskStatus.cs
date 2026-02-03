namespace GitProtect.Models;

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
