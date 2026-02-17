using CLI.Configuration;
using CLI.Runtime;
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
        AppLogger.Info("Starting scheduled backup and mirror job loops.");

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
            AppLogger.Debug("{JobName}: evaluating cron expression '{CronExpression}'.", jobName, cronExpression);
            if (!CronScheduleParser.TryParse(cronExpression, out var schedule, out var parseError) || schedule is null)
            {
                if (!string.Equals(lastInvalidCron, cronExpression, StringComparison.Ordinal))
                {
                    AppLogger.Warn(
                        "{JobName}: schedule '{CronExpression}' is invalid ({ParseError}). Waiting for configuration reload.",
                        jobName,
                        cronExpression,
                        parseError);
                    lastInvalidCron = cronExpression;
                }

                await Task.Delay(TimeSpan.FromSeconds(1), cancellationToken);
                continue;
            }

            lastInvalidCron = null;
            if (!string.Equals(lastAppliedCron, cronExpression, StringComparison.Ordinal))
            {
                AppLogger.Info("{JobName}: active schedule is '{CronExpression}'.", jobName, cronExpression);
                lastAppliedCron = cronExpression;
            }

            var now = DateTimeOffset.UtcNow;
            var nextOccurrence = schedule.GetNextOccurrence(now.AddMilliseconds(1), TimeZoneInfo.Utc);

            if (nextOccurrence is null)
            {
                AppLogger.Error(
                    "{JobName}: schedule '{CronExpression}' has no next occurrence. Stopping this job loop.",
                    jobName,
                    cronExpression);
                return;
            }

            var secondsUntilNextRun = Math.Max(0L, (long)Math.Ceiling((nextOccurrence.Value - now).TotalSeconds));
            AppLogger.Info(
                "{JobName}: next run at {NextRunTimestamp} (in {SecondsUntilNextRun} seconds).",
                jobName,
                AppLogger.FormatTimestamp(nextOccurrence.Value),
                secondsUntilNextRun);
            var waitResult = await DelayUntilUtcAsync(
                jobName,
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
                AppLogger.Info(
                    "{JobName}: schedule changed from '{PreviousCronExpression}' to '{CurrentCronExpression}'. Recomputing next run.",
                    jobName,
                    cronExpression,
                    updatedCronExpression);
                continue;
            }

            var runStartedAt = DateTimeOffset.UtcNow;
            AppLogger.Info("{JobName}: run started.", jobName);

            try
            {
                await runJob(cancellationToken);
                var runDurationSeconds = (DateTimeOffset.UtcNow - runStartedAt).TotalSeconds;
                AppLogger.Info(
                    "{JobName}: run completed in {DurationSeconds:0.###} seconds.",
                    jobName,
                    runDurationSeconds);
            }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
            {
                return;
            }
            catch (Exception exception)
            {
                var runDurationSeconds = (DateTimeOffset.UtcNow - runStartedAt).TotalSeconds;
                AppLogger.Error(
                    exception,
                    "{JobName}: run failed after {DurationSeconds:0.###} seconds. {ErrorMessage}",
                    jobName,
                    runDurationSeconds,
                    exception.Message);
            }

            if (!cancellationToken.IsCancellationRequested)
            {
                await RunRetentionAsync(jobName, cancellationToken);
            }
        }
    }

    private static async Task<DelayUntilUtcResult> DelayUntilUtcAsync(
        string jobName,
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
                AppLogger.Debug(
                    "{JobName}: detected schedule change while waiting (old='{OldCronExpression}', new='{NewCronExpression}').",
                    jobName,
                    scheduledCronExpression,
                    currentCronExpression);
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
            AppLogger.Info("Retention started after the {TriggeredByJob} job run.", triggeredBy);
            await _retentionService.RunAsync(_getSettings(), cancellationToken);
            AppLogger.Info("Retention completed.");
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            // Exit cleanly on shutdown.
            AppLogger.Warn("Retention cancelled because shutdown was requested.");
        }
        catch (Exception exception)
        {
            AppLogger.Error(
                exception,
                "Retention failed after the {TriggeredByJob} job run. {ErrorMessage}",
                triggeredBy,
                exception.Message);
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
