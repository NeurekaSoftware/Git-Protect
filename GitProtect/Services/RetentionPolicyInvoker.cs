using Coravel.Invocable;
using GitProtect.Data;
using Microsoft.EntityFrameworkCore;

namespace GitProtect.Services;

public sealed class RetentionPolicyInvoker : IInvocable
{
    private readonly GitProtectDbContext _db;
    private readonly S3StorageService _storageService;
    private readonly ILogger<RetentionPolicyInvoker> _logger;

    public RetentionPolicyInvoker(GitProtectDbContext db, S3StorageService storageService, ILogger<RetentionPolicyInvoker> logger)
    {
        _db = db;
        _storageService = storageService;
        _logger = logger;
    }

    public async Task Invoke()
    {
        var policy = await _db.RetentionPolicies.AsNoTracking().FirstOrDefaultAsync();
        if (policy is null || !policy.IsEnabled)
        {
            return;
        }

        if (policy.RetentionDays <= 0)
        {
            _logger.LogWarning("Retention policy is enabled but has an invalid retention window: {Days}.", policy.RetentionDays);
            return;
        }

        var storage = await _db.S3Configs.AsNoTracking().FirstOrDefaultAsync();
        if (storage is null)
        {
            _logger.LogWarning("Retention policy is enabled but storage is not configured.");
            return;
        }

        var cutoff = DateTimeOffset.UtcNow.AddDays(-policy.RetentionDays);
        var deleted = await _storageService.DeleteBackupsOlderThanAsync(storage, cutoff, CancellationToken.None);

        if (deleted > 0)
        {
            _logger.LogInformation("Retention policy deleted {Count} backup object(s) older than {Cutoff}.", deleted, cutoff);
        }
    }
}
