using GitProtect.Models;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace GitProtect.Services;

public sealed class BackupWorker : BackgroundService
{
    private readonly BackupQueue _queue;
    private readonly IServiceScopeFactory _scopeFactory;
    private readonly ILogger<BackupWorker> _logger;

    public BackupWorker(BackupQueue queue, IServiceScopeFactory scopeFactory, ILogger<BackupWorker> logger)
    {
        _queue = queue;
        _scopeFactory = scopeFactory;
        _logger = logger;
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            var job = await _queue.DequeueAsync(stoppingToken);
            using var scope = _scopeFactory.CreateScope();
            var backupService = scope.ServiceProvider.GetRequiredService<BackupService>();

            try
            {
                if (job.RepositoryId.HasValue)
                {
                    await backupService.RunRepositoryBackupAsync(job.TaskId, job.RepositoryId.Value, stoppingToken);
                }
                else
                {
                    await backupService.RunProviderBackupAsync(job.TaskId, job.Provider, stoppingToken);
                }
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Backup job failed.");
                await backupService.MarkTaskFailedAsync(job.TaskId, ex.Message, stoppingToken);
            }
        }
    }
}
