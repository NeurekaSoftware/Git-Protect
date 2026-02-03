using System.ComponentModel.DataAnnotations;
using GitProtect.Data;
using GitProtect.Models;
using GitProtect.Services;
using GitProtect.Validation;
using Microsoft.EntityFrameworkCore;

namespace GitProtect.Endpoints;

public static class ApiEndpoints
{
    private const int DefaultRetentionDays = 30;

    public static IEndpointRouteBuilder MapGitProtectApi(this IEndpointRouteBuilder app)
    {
        var api = app.MapGroup("/api");

        api.MapGet("/status", async (GitProtectDbContext db) =>
        {
            var providers = await EnsureProvidersAsync(db);
            var storage = await db.S3Configs.AsNoTracking().FirstOrDefaultAsync();

            return new SetupStatusDto(
                storage is not null,
                storage?.IsVerified == true,
                providers.Select(ToProviderStatusDto).ToList());
        });

        api.MapGet("/providers", async (GitProtectDbContext db) =>
        {
            var providers = await EnsureProvidersAsync(db);
            return providers.Select(ToProviderStatusDto).ToList();
        });

        api.MapPut("/providers/{provider}", async (
            string provider,
            ProviderUpsertRequest request,
            GitProtectDbContext db,
            ProviderApiService providerApi,
            CancellationToken cancellationToken) =>
        {
            if (!Enum.TryParse<ProviderType>(provider, true, out var providerType))
            {
                return Results.BadRequest(new { message = "Unknown provider." });
            }

            var config = await GetOrCreateProviderAsync(db, providerType);
            config.BaseUrl = request.BaseUrl.Trim();
            config.Username = request.Username.Trim();
            if (!string.IsNullOrWhiteSpace(request.ApiToken))
            {
                config.ApiToken = request.ApiToken.Trim();
            }
            config.IsConfigured = true;

            var isValid = await providerApi.ValidateAsync(config, cancellationToken);
            config.IsVerified = isValid;
            config.VerifiedAt = isValid ? DateTime.UtcNow : null;

            await db.SaveChangesAsync(cancellationToken);

            if (!isValid)
            {
                return Results.BadRequest(new { message = "Provider validation failed." });
            }

            return Results.Ok(ToProviderStatusDto(config));
        });

        api.MapPost("/providers/{provider}/sync", async (
            string provider,
            GitProtectDbContext db,
            ProviderApiService providerApi,
            CancellationToken cancellationToken) =>
        {
            if (!Enum.TryParse<ProviderType>(provider, true, out var providerType))
            {
                return Results.BadRequest(new { message = "Unknown provider." });
            }

            var config = await db.ProviderConfigs.FirstOrDefaultAsync(p => p.Provider == providerType, cancellationToken);
            if (config is null || !config.IsVerified)
            {
                return Results.BadRequest(new { message = "Provider not configured." });
            }

            var remoteRepos = await providerApi.FetchRepositoriesAsync(config, cancellationToken);
            var existing = await db.Repositories.Where(r => r.Provider == providerType).ToListAsync(cancellationToken);

            foreach (var remote in remoteRepos)
            {
                var repo = existing.FirstOrDefault(r => r.ExternalId == remote.ExternalId);
                if (repo is null)
                {
                    repo = new RepositoryRecord
                    {
                        Provider = providerType,
                        ExternalId = remote.ExternalId,
                        Name = remote.Name,
                        FullName = remote.FullName,
                        CloneUrl = remote.CloneUrl,
                        DefaultBranch = remote.DefaultBranch
                    };
                    db.Repositories.Add(repo);
                }
                else
                {
                    repo.Name = remote.Name;
                    repo.FullName = remote.FullName;
                    repo.CloneUrl = remote.CloneUrl;
                    repo.DefaultBranch = remote.DefaultBranch;
                }
            }

            var remoteIds = remoteRepos.Select(r => r.ExternalId).ToHashSet();
            foreach (var repo in existing.Where(r => !remoteIds.Contains(r.ExternalId)))
            {
                db.Repositories.Remove(repo);
            }

            config.LastSyncAt = DateTime.UtcNow;
            await db.SaveChangesAsync(cancellationToken);

            return Results.Ok(new { count = remoteRepos.Count });
        });

        api.MapGet("/providers/{provider}/repos", async (string provider, GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            if (!Enum.TryParse<ProviderType>(provider, true, out var providerType))
            {
                return Results.BadRequest(new { message = "Unknown provider." });
            }

            var repos = await db.Repositories
                .AsNoTracking()
                .Where(r => r.Provider == providerType)
                .OrderBy(r => r.FullName)
                .ToListAsync(cancellationToken);

            return Results.Ok(repos.Select(ToRepositoryDto).ToList());
        });

        api.MapPost("/providers/{provider}/backup", async (
            string provider,
            GitProtectDbContext db,
            BackupQueue queue,
            CancellationToken cancellationToken) =>
        {
            if (!Enum.TryParse<ProviderType>(provider, true, out var providerType))
            {
                return Results.BadRequest(new { message = "Unknown provider." });
            }

            if (await IsProviderBackupInProgressAsync(db, providerType, cancellationToken))
            {
                return Results.Conflict(new { message = "Backup already running for this provider." });
            }

            var task = new BackupTask
            {
                Provider = providerType,
                Name = $"{providerType} Backup",
                Status = BackupTaskStatus.Pending,
                Progress = 0
            };

            db.BackupTasks.Add(task);
            await db.SaveChangesAsync(cancellationToken);

            await queue.EnqueueAsync(new BackupJob(task.Id, providerType, null));
            return Results.Ok(ToTaskDto(task));
        });

        api.MapPost("/providers/{provider}/backup/{repoId:int}", async (
            string provider,
            int repoId,
            GitProtectDbContext db,
            BackupQueue queue,
            CancellationToken cancellationToken) =>
        {
            if (!Enum.TryParse<ProviderType>(provider, true, out var providerType))
            {
                return Results.BadRequest(new { message = "Unknown provider." });
            }

            if (await IsRepositoryBackupInProgressAsync(db, providerType, repoId, cancellationToken))
            {
                return Results.Conflict(new { message = "Backup already running for this repository." });
            }

            var repo = await db.Repositories.FirstOrDefaultAsync(r => r.Id == repoId && r.Provider == providerType, cancellationToken);
            if (repo is null)
            {
                return Results.NotFound();
            }

            var task = new BackupTask
            {
                Provider = providerType,
                RepositoryId = repoId,
                Name = $"{repo.FullName} Backup",
                Status = BackupTaskStatus.Pending,
                Progress = 0
            };

            db.BackupTasks.Add(task);
            await db.SaveChangesAsync(cancellationToken);

            await queue.EnqueueAsync(new BackupJob(task.Id, providerType, repoId));
            return Results.Ok(ToTaskDto(task));
        });

        api.MapGet("/tasks", async (int? limit, GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            IQueryable<BackupTask> query = db.BackupTasks
                .AsNoTracking()
                .OrderByDescending(t => t.Id);

            if (limit.HasValue)
            {
                var safeLimit = Math.Clamp(limit.Value, 1, 1000);
                query = query.Take(safeLimit);
            }

            var tasks = await query.ToListAsync(cancellationToken);
            return tasks.Select(ToTaskDto).ToList();
        });

        api.MapGet("/storage", async (GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            var storage = await db.S3Configs.AsNoTracking().FirstOrDefaultAsync(cancellationToken);
            var totalBytes = await db.Repositories.SumAsync(r => r.LastBackupSizeBytes ?? 0, cancellationToken);
            return new StorageDetailsDto(storage is null ? null : ToS3ConfigDto(storage), totalBytes, storage?.VerifiedAt);
        });

        api.MapPut("/storage", async (
            S3UpsertRequest request,
            GitProtectDbContext db,
            S3StorageService storageService,
            CancellationToken cancellationToken) =>
        {
            var config = await db.S3Configs.FirstOrDefaultAsync(cancellationToken) ?? new S3Config();
            config.Endpoint = request.Endpoint.Trim();
            config.Region = request.Region.Trim();
            config.Bucket = request.Bucket.Trim();
            config.UsePathStyle = request.UsePathStyle;
            if (!string.IsNullOrWhiteSpace(request.AccessKeyId))
            {
                config.AccessKeyId = request.AccessKeyId.Trim();
            }
            if (!string.IsNullOrWhiteSpace(request.SecretAccessKey))
            {
                config.SecretAccessKey = request.SecretAccessKey.Trim();
            }

            if (config.Id == 0)
            {
                db.S3Configs.Add(config);
            }

            try
            {
                await storageService.VerifyAsync(config, cancellationToken);
                config.IsVerified = true;
                config.VerifiedAt = DateTime.UtcNow;
            }
            catch (Exception ex)
            {
                config.IsVerified = false;
                config.VerifiedAt = null;
                await db.SaveChangesAsync(cancellationToken);
                return Results.BadRequest(new { message = ex.Message });
            }

            await db.SaveChangesAsync(cancellationToken);
            return Results.Ok(ToS3ConfigDto(config));
        });

        api.MapGet("/schedule", async (GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            var schedule = await db.BackupSchedules.AsNoTracking().FirstOrDefaultAsync(cancellationToken);
            return schedule is null
                ? new BackupScheduleDto(false, CronExpressionValidator.DefaultDailyExpression)
                : new BackupScheduleDto(schedule.IsEnabled, schedule.CronExpression);
        });

        api.MapPut("/schedule", async (
            BackupScheduleUpsertRequest request,
            GitProtectDbContext db,
            CancellationToken cancellationToken) =>
        {
            if (!TryValidate(request, out var error))
            {
                return Results.BadRequest(new { message = error ?? "Invalid schedule settings." });
            }

            var schedule = await db.BackupSchedules.FirstOrDefaultAsync(cancellationToken) ?? new BackupSchedule();
            schedule.IsEnabled = request.IsEnabled;
            schedule.CronExpression = request.CronExpression.Trim();

            if (schedule.Id == 0)
            {
                db.BackupSchedules.Add(schedule);
            }

            await db.SaveChangesAsync(cancellationToken);

            return Results.Ok(new BackupScheduleDto(schedule.IsEnabled, schedule.CronExpression));
        });

        api.MapGet("/retention", async (GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            var policy = await db.RetentionPolicies.AsNoTracking().FirstOrDefaultAsync(cancellationToken);
            return policy is null
                ? new RetentionPolicyDto(false, DefaultRetentionDays)
                : new RetentionPolicyDto(policy.IsEnabled, policy.RetentionDays);
        });

        api.MapPut("/retention", async (
            RetentionPolicyUpsertRequest request,
            GitProtectDbContext db,
            CancellationToken cancellationToken) =>
        {
            if (!TryValidate(request, out var error))
            {
                return Results.BadRequest(new { message = error ?? "Invalid retention settings." });
            }

            var policy = await db.RetentionPolicies.FirstOrDefaultAsync(cancellationToken) ?? new RetentionPolicy();
            policy.IsEnabled = request.IsEnabled;
            policy.RetentionDays = request.RetentionDays;

            if (policy.Id == 0)
            {
                db.RetentionPolicies.Add(policy);
            }

            await db.SaveChangesAsync(cancellationToken);

            return Results.Ok(new RetentionPolicyDto(policy.IsEnabled, policy.RetentionDays));
        });

        api.MapGet("/dashboard", async (GitProtectDbContext db, CancellationToken cancellationToken) =>
        {
            var total = await db.Repositories.CountAsync(cancellationToken);
            var protectedCount = await db.Repositories.CountAsync(r => r.LastBackupStatus == BackupStatus.Success, cancellationToken);
            var failedCount = await db.Repositories.CountAsync(r => r.LastBackupStatus == BackupStatus.Failed, cancellationToken);
            var pendingCount = await db.Repositories.CountAsync(r => r.LastBackupStatus == BackupStatus.Never || r.LastBackupStatus == BackupStatus.Running, cancellationToken);

            var start = DateOnly.FromDateTime(DateTime.UtcNow).AddDays(-6);
            var completed = await db.BackupTasks
                .AsNoTracking()
                .Where(t => t.CompletedAt != null)
                .OrderByDescending(t => t.Id)
                .Select(t => t.CompletedAt)
                .Take(2000)
                .ToListAsync(cancellationToken);
            var activity = new List<ChartPointDto>();
            for (var i = 0; i < 7; i++)
            {
                var day = start.AddDays(i);
                var next = day.AddDays(1);
                var dayStart = DateTime.SpecifyKind(day.ToDateTime(TimeOnly.MinValue), DateTimeKind.Utc);
                var nextStart = DateTime.SpecifyKind(next.ToDateTime(TimeOnly.MinValue), DateTimeKind.Utc);

                var count = completed.Count(t => t.HasValue && t.Value >= dayStart && t.Value < nextStart);

                activity.Add(new ChartPointDto(day.ToString("ddd"), count));
            }

            return new DashboardDto(total, protectedCount, failedCount, pendingCount, activity);
        });

        return app;
    }

    private static ProviderStatusDto ToProviderStatusDto(ProviderConfig config)
    {
        return new ProviderStatusDto(
            config.Provider,
            config.BaseUrl,
            config.Username,
            config.IsConfigured,
            config.IsVerified,
            config.VerifiedAt,
            config.LastSyncAt);
    }

    private static RepositoryDto ToRepositoryDto(RepositoryRecord repo)
    {
        return new RepositoryDto(
            repo.Id,
            repo.Name,
            repo.FullName,
            repo.LastBackupStatus,
            repo.LastBackupAt,
            repo.LastBackupSizeBytes,
            repo.LastBackupMessage);
    }

    private static BackupTaskDto ToTaskDto(BackupTask task)
    {
        return new BackupTaskDto(
            task.Id,
            task.Name,
            task.Provider,
            task.RepositoryId,
            task.Status,
            task.Progress,
            task.CreatedAt,
            task.StartedAt,
            task.CompletedAt,
            task.Message);
    }

    private static S3ConfigDto ToS3ConfigDto(S3Config config)
    {
        return new S3ConfigDto(
            config.Endpoint,
            config.Region,
            config.Bucket,
            config.AccessKeyId,
            string.Empty,
            config.UsePathStyle,
            config.IsVerified,
            config.VerifiedAt);
    }

    private static bool TryValidate<T>(T request, out string? error)
        where T : class
    {
        var results = new List<ValidationResult>();
        var isValid = Validator.TryValidateObject(request, new ValidationContext(request), results, true);
        error = results.FirstOrDefault()?.ErrorMessage;
        return isValid;
    }

    private static async Task<List<ProviderConfig>> EnsureProvidersAsync(GitProtectDbContext db)
    {
        foreach (var provider in Enum.GetValues<ProviderType>())
        {
            await GetOrCreateProviderAsync(db, provider);
        }

        await db.SaveChangesAsync();
        return await db.ProviderConfigs.AsNoTracking().OrderBy(p => p.Provider).ToListAsync();
    }

    private static async Task<ProviderConfig> GetOrCreateProviderAsync(GitProtectDbContext db, ProviderType provider)
    {
        var existing = await db.ProviderConfigs.FirstOrDefaultAsync(p => p.Provider == provider);
        if (existing != null)
        {
            return existing;
        }

        var config = new ProviderConfig
        {
            Provider = provider,
            BaseUrl = provider switch
            {
                ProviderType.GitHub => "https://github.com",
                ProviderType.GitLab => "https://gitlab.com",
                ProviderType.Forgejo => "https://codeberg.org",
                _ => string.Empty
            }
        };

        db.ProviderConfigs.Add(config);
        return config;
    }

    private static Task<bool> IsProviderBackupInProgressAsync(GitProtectDbContext db, ProviderType provider, CancellationToken cancellationToken)
    {
        return db.BackupTasks.AnyAsync(t =>
            t.Provider == provider
            && t.RepositoryId == null
            && (t.Status == BackupTaskStatus.Pending || t.Status == BackupTaskStatus.Running),
            cancellationToken);
    }

    private static Task<bool> IsRepositoryBackupInProgressAsync(GitProtectDbContext db, ProviderType provider, int repoId, CancellationToken cancellationToken)
    {
        return db.BackupTasks.AnyAsync(t =>
            t.Provider == provider
            && t.RepositoryId == repoId
            && (t.Status == BackupTaskStatus.Pending || t.Status == BackupTaskStatus.Running),
            cancellationToken);
    }
}
