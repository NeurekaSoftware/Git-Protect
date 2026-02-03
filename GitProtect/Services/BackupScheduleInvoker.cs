using Coravel.Invocable;
using GitProtect.Data;
using GitProtect.Models;
using GitProtect.Validation;
using Microsoft.EntityFrameworkCore;

namespace GitProtect.Services;

public sealed class BackupScheduleInvoker : IInvocable
{
    private readonly GitProtectDbContext _db;
    private readonly BackupQueue _queue;
    private readonly ILogger<BackupScheduleInvoker> _logger;

    public BackupScheduleInvoker(GitProtectDbContext db, BackupQueue queue, ILogger<BackupScheduleInvoker> logger)
    {
        _db = db;
        _queue = queue;
        _logger = logger;
    }

    public async Task Invoke()
    {
        var schedule = await _db.BackupSchedules.AsNoTracking().FirstOrDefaultAsync();
        if (schedule is null || !schedule.IsEnabled)
        {
            return;
        }

        var now = DateTimeOffset.Now;
        if (!CronExpressionValidator.IsDue(schedule.CronExpression, now))
        {
            return;
        }

        var providers = await _db.ProviderConfigs
            .AsNoTracking()
            .Where(p => p.IsVerified)
            .ToListAsync();

        if (providers.Count == 0)
        {
            return;
        }

        var activeProviders = await _db.BackupTasks
            .AsNoTracking()
            .Where(t => t.RepositoryId == null
                && (t.Status == BackupTaskStatus.Pending || t.Status == BackupTaskStatus.Running))
            .Select(t => t.Provider)
            .ToListAsync();

        var activeSet = activeProviders.ToHashSet();
        var newTasks = new List<BackupTask>();

        foreach (var provider in providers)
        {
            if (activeSet.Contains(provider.Provider))
            {
                continue;
            }

            var task = new BackupTask
            {
                Provider = provider.Provider,
                Name = $"{provider.Provider} Backup",
                Status = BackupTaskStatus.Pending,
                Progress = 0
            };

            newTasks.Add(task);
            _db.BackupTasks.Add(task);
        }

        if (newTasks.Count == 0)
        {
            return;
        }

        await _db.SaveChangesAsync();

        foreach (var task in newTasks)
        {
            await _queue.EnqueueAsync(new BackupJob(task.Id, task.Provider, null));
        }

        _logger.LogInformation("Scheduled backup queued for {Count} providers.", newTasks.Count);
    }
}
