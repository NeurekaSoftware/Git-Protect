using CLI.Configuration;
using CLI.Services.Backup;

namespace CLI.Services.Scheduling;

public sealed class ScheduledJobRunner
{
    private readonly Func<Settings> _getSettings;
    private readonly MirrorService _mirrorService;
    private readonly BackupService _backupService;
    private readonly RetentionService _retentionService;
    private readonly SemaphoreSlim _retentionLock = new(1, 1);

    public ScheduledJobRunner(
        Func<Settings> getSettings,
        MirrorService mirrorService,
        BackupService backupService,
        RetentionService retentionService)
    {
        _getSettings = getSettings;
        _mirrorService = mirrorService;
        _backupService = backupService;
        _retentionService = retentionService;
    }

    public async Task RunForeverAsync(CancellationToken cancellationToken)
    {
        var backupLoop = RunScheduledLoopAsync(
            jobName: "backups",
            getCronExpression: () => _getSettings().Schedule.Backups.Cron,
            runJob: token => _backupService.RunAsync(_getSettings(), token),
            cancellationToken);

        var mirrorLoop = RunScheduledLoopAsync(
            jobName: "mirrors",
            getCronExpression: () => _getSettings().Schedule.Mirrors.Cron,
            runJob: token => _mirrorService.RunAsync(_getSettings(), token),
            cancellationToken);

        await Task.WhenAll(backupLoop, mirrorLoop);
    }

    private async Task RunScheduledLoopAsync(
        string jobName,
        Func<string?> getCronExpression,
        Func<CancellationToken, Task> runJob,
        CancellationToken cancellationToken)
    {
        string? lastAppliedCron = null;
        string? lastInvalidCron = null;

        while (!cancellationToken.IsCancellationRequested)
        {
            var cronExpression = getCronExpression();
            if (!CronScheduleParser.TryParse(cronExpression, out var schedule, out var parseError) || schedule is null)
            {
                if (!string.Equals(lastInvalidCron, cronExpression, StringComparison.Ordinal))
                {
                    Console.Error.WriteLine($"{jobName}: invalid schedule '{cronExpression}': {parseError}. Waiting for config reload.");
                    lastInvalidCron = cronExpression;
                }

                await Task.Delay(TimeSpan.FromSeconds(1), cancellationToken);
                continue;
            }

            lastInvalidCron = null;
            if (!string.Equals(lastAppliedCron, cronExpression, StringComparison.Ordinal))
            {
                Console.WriteLine($"{jobName}: schedule set to {cronExpression}");
                lastAppliedCron = cronExpression;
            }

            var now = DateTimeOffset.UtcNow;
            var nextOccurrence = schedule.GetNextOccurrence(now.AddMilliseconds(1), TimeZoneInfo.Utc);

            if (nextOccurrence is null)
            {
                Console.Error.WriteLine($"{jobName}: schedule has no next occurrence. Stopping this job loop.");
                return;
            }

            Console.WriteLine($"{jobName}: next run at {nextOccurrence.Value:O}.");
            var waitResult = await DelayUntilUtcAsync(
                nextOccurrence.Value,
                cronExpression,
                getCronExpression,
                cancellationToken);

            if (waitResult == DelayUntilUtcResult.Cancelled)
            {
                return;
            }

            if (waitResult == DelayUntilUtcResult.RescheduleRequested)
            {
                var updatedCronExpression = getCronExpression();
                Console.WriteLine(
                    $"{jobName}: schedule changed from '{cronExpression}' to '{updatedCronExpression}'. Recomputing next run.");
                continue;
            }

            Console.WriteLine($"{jobName}: run started at {DateTimeOffset.UtcNow:O}.");

            try
            {
                await runJob(cancellationToken);
                Console.WriteLine($"{jobName}: run completed at {DateTimeOffset.UtcNow:O}.");
            }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
            {
                return;
            }
            catch (Exception exception)
            {
                Console.Error.WriteLine($"{jobName}: run failed: {exception.Message}");
            }

            if (!cancellationToken.IsCancellationRequested)
            {
                await RunRetentionAsync(jobName, cancellationToken);
            }
        }
    }

    private static async Task<DelayUntilUtcResult> DelayUntilUtcAsync(
        DateTimeOffset target,
        string? scheduledCronExpression,
        Func<string?> getCronExpression,
        CancellationToken cancellationToken)
    {
        while (!cancellationToken.IsCancellationRequested)
        {
            var currentCronExpression = getCronExpression();
            if (!string.Equals(currentCronExpression, scheduledCronExpression, StringComparison.Ordinal))
            {
                return DelayUntilUtcResult.RescheduleRequested;
            }

            var remaining = target - DateTimeOffset.UtcNow;
            if (remaining <= TimeSpan.Zero)
            {
                return DelayUntilUtcResult.TargetReached;
            }

            var delay = remaining > TimeSpan.FromSeconds(1)
                ? TimeSpan.FromSeconds(1)
                : remaining;

            try
            {
                await Task.Delay(delay, cancellationToken);
            }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
            {
                return DelayUntilUtcResult.Cancelled;
            }
        }

        return DelayUntilUtcResult.Cancelled;
    }

    private async Task RunRetentionAsync(string triggeredBy, CancellationToken cancellationToken)
    {
        await _retentionLock.WaitAsync(cancellationToken);

        try
        {
            Console.WriteLine($"retention: started after {triggeredBy} run.");
            await _retentionService.RunAsync(_getSettings(), cancellationToken);
            Console.WriteLine("retention: completed.");
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            // Exit cleanly on shutdown.
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"retention: failed after {triggeredBy} run: {exception.Message}");
        }
        finally
        {
            _retentionLock.Release();
        }
    }

    private enum DelayUntilUtcResult
    {
        TargetReached,
        RescheduleRequested,
        Cancelled
    }
}
