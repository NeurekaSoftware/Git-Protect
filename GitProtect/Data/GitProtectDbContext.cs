using GitProtect.Models;
using Microsoft.EntityFrameworkCore;

namespace GitProtect.Data;

public sealed class GitProtectDbContext : DbContext
{
    public GitProtectDbContext(DbContextOptions<GitProtectDbContext> options) : base(options)
    {
    }

    public DbSet<ProviderConfig> ProviderConfigs => Set<ProviderConfig>();
    public DbSet<S3Config> S3Configs => Set<S3Config>();
    public DbSet<BackupSchedule> BackupSchedules => Set<BackupSchedule>();
    public DbSet<RetentionPolicy> RetentionPolicies => Set<RetentionPolicy>();
    public DbSet<RepositoryRecord> Repositories => Set<RepositoryRecord>();
    public DbSet<BackupTask> BackupTasks => Set<BackupTask>();

    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<ProviderConfig>()
            .HasIndex(p => p.Provider)
            .IsUnique();

        modelBuilder.Entity<RepositoryRecord>()
            .HasIndex(r => new { r.Provider, r.ExternalId })
            .IsUnique();

        modelBuilder.Entity<ProviderConfig>()
            .Property(p => p.Provider)
            .HasConversion<string>();

        modelBuilder.Entity<RepositoryRecord>()
            .Property(r => r.Provider)
            .HasConversion<string>();

        modelBuilder.Entity<RepositoryRecord>()
            .Property(r => r.LastBackupStatus)
            .HasConversion<string>();

        modelBuilder.Entity<BackupTask>()
            .Property(t => t.Provider)
            .HasConversion<string>();

        modelBuilder.Entity<BackupTask>()
            .Property(t => t.Status)
            .HasConversion<string>();
    }
}
