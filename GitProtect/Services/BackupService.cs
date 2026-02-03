using GitProtect.Data;
using GitProtect.Models;
using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Logging;

namespace GitProtect.Services;

public sealed class BackupService
{
    private readonly GitProtectDbContext _db;
    private readonly GitMirrorService _gitMirrorService;
    private readonly S3StorageService _storageService;
    private readonly ILogger<BackupService> _logger;

    public BackupService(GitProtectDbContext db, GitMirrorService gitMirrorService, S3StorageService storageService, ILogger<BackupService> logger)
    {
        _db = db;
        _gitMirrorService = gitMirrorService;
        _storageService = storageService;
        _logger = logger;
    }

    public async Task RunProviderBackupAsync(int taskId, ProviderType provider, CancellationToken cancellationToken)
    {
        var task = await _db.BackupTasks.FirstAsync(t => t.Id == taskId, cancellationToken);
        var providerConfig = await _db.ProviderConfigs.FirstAsync(p => p.Provider == provider, cancellationToken);
        var storage = await _db.S3Configs.FirstAsync(cancellationToken);
        var repos = await _db.Repositories.Where(r => r.Provider == provider).ToListAsync(cancellationToken);

        task.Status = BackupTaskStatus.Running;
        task.StartedAt = DateTimeOffset.UtcNow;
        await _db.SaveChangesAsync(cancellationToken);

        if (repos.Count == 0)
        {
            task.Status = BackupTaskStatus.Success;
            task.Progress = 100;
            task.CompletedAt = DateTimeOffset.UtcNow;
            task.Message = "No repositories to back up.";
            await _db.SaveChangesAsync(cancellationToken);
            return;
        }

        var runTimestamp = DateTimeOffset.UtcNow;
        var runId = Guid.CreateVersion7();
        var prefixRoot = BuildPrefixRoot(runTimestamp, runId);
        var failures = 0;

        for (var index = 0; index < repos.Count; index++)
        {
            var repo = repos[index];
            try
            {
                repo.LastBackupStatus = BackupStatus.Running;
                await _db.SaveChangesAsync(cancellationToken);

                var mirrorResult = await _gitMirrorService.CreateOrUpdateMirrorAsync(providerConfig, repo, cancellationToken);
                var keyPrefix = $"{prefixRoot}{providerConfig.Provider.ToString().ToLowerInvariant()}/{repo.FullName}/";
                var sizeBytes = await _storageService.UploadDirectoryAsync(storage, mirrorResult.Path, keyPrefix, cancellationToken);

                repo.LastBackupStatus = BackupStatus.Success;
                repo.LastBackupAt = DateTimeOffset.UtcNow;
                repo.LastBackupSizeBytes = sizeBytes;
                repo.LastBackupMessage = null;
            }
            catch (Exception ex)
            {
                failures++;
                repo.LastBackupStatus = BackupStatus.Failed;
                repo.LastBackupAt = DateTimeOffset.UtcNow;
                repo.LastBackupMessage = ex.Message;
                _logger.LogError(ex, "Backup failed for {Repo}", repo.FullName);
            }

            task.Progress = repos.Count == 0 ? 100 : (int)Math.Round(((index + 1) / (double)repos.Count) * 100);
            await _db.SaveChangesAsync(cancellationToken);
        }

        task.Status = failures == 0 ? BackupTaskStatus.Success : BackupTaskStatus.Failed;
        task.CompletedAt = DateTimeOffset.UtcNow;
        task.Message = failures == 0 ? "Completed" : $"{failures} repo(s) failed";
        await _db.SaveChangesAsync(cancellationToken);
    }

    public async Task RunRepositoryBackupAsync(int taskId, int repositoryId, CancellationToken cancellationToken)
    {
        var task = await _db.BackupTasks.FirstAsync(t => t.Id == taskId, cancellationToken);
        var repo = await _db.Repositories.FirstAsync(r => r.Id == repositoryId, cancellationToken);
        var providerConfig = await _db.ProviderConfigs.FirstAsync(p => p.Provider == repo.Provider, cancellationToken);
        var storage = await _db.S3Configs.FirstAsync(cancellationToken);

        task.Status = BackupTaskStatus.Running;
        task.StartedAt = DateTimeOffset.UtcNow;
        task.Progress = 0;
        repo.LastBackupStatus = BackupStatus.Running;
        repo.LastBackupMessage = null;
        await _db.SaveChangesAsync(cancellationToken);

        try
        {
            var runTimestamp = DateTimeOffset.UtcNow;
            var runId = Guid.CreateVersion7();
            var prefixRoot = BuildPrefixRoot(runTimestamp, runId);
            var mirrorResult = await _gitMirrorService.CreateOrUpdateMirrorAsync(providerConfig, repo, cancellationToken);
            var keyPrefix = $"{prefixRoot}{providerConfig.Provider.ToString().ToLowerInvariant()}/{repo.FullName}/";
            var sizeBytes = await _storageService.UploadDirectoryAsync(storage, mirrorResult.Path, keyPrefix, cancellationToken);

            repo.LastBackupStatus = BackupStatus.Success;
            repo.LastBackupAt = DateTimeOffset.UtcNow;
            repo.LastBackupSizeBytes = sizeBytes;
            repo.LastBackupMessage = null;

            task.Progress = 100;
            task.Status = BackupTaskStatus.Success;
            task.CompletedAt = DateTimeOffset.UtcNow;
            task.Message = "Completed";
        }
        catch (Exception ex)
        {
            repo.LastBackupStatus = BackupStatus.Failed;
            repo.LastBackupAt = DateTimeOffset.UtcNow;
            repo.LastBackupMessage = ex.Message;

            task.Status = BackupTaskStatus.Failed;
            task.CompletedAt = DateTimeOffset.UtcNow;
            task.Message = ex.Message;
        }

        await _db.SaveChangesAsync(cancellationToken);
    }

    public async Task MarkTaskFailedAsync(int taskId, string message, CancellationToken cancellationToken)
    {
        var task = await _db.BackupTasks.FirstOrDefaultAsync(t => t.Id == taskId, cancellationToken);
        if (task == null)
        {
            return;
        }

        task.Status = BackupTaskStatus.Failed;
        task.CompletedAt = DateTimeOffset.UtcNow;
        task.Message = message;
        await _db.SaveChangesAsync(cancellationToken);
    }

    private static string BuildPrefixRoot(DateTimeOffset timestamp, Guid runId)
    {
        return $"{timestamp:yyyy/MM}/{runId}/";
    }
}
