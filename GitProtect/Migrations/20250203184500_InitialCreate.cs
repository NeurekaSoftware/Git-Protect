using System;
using Microsoft.EntityFrameworkCore.Infrastructure;
using Microsoft.EntityFrameworkCore.Migrations;

#nullable disable

namespace GitProtect.Migrations;

[DbContext(typeof(GitProtect.Data.GitProtectDbContext))]
[Migration("20250203184500_InitialCreate")]
public partial class InitialCreate : Migration
{
    protected override void Up(MigrationBuilder migrationBuilder)
    {
        migrationBuilder.CreateTable(
            name: "BackupTasks",
            columns: table => new
            {
                Id = table.Column<int>(type: "INTEGER", nullable: false)
                    .Annotation("Sqlite:Autoincrement", true),
                Provider = table.Column<string>(type: "TEXT", nullable: false),
                RepositoryId = table.Column<int>(type: "INTEGER", nullable: true),
                Name = table.Column<string>(type: "TEXT", nullable: false),
                Status = table.Column<string>(type: "TEXT", nullable: false),
                Progress = table.Column<int>(type: "INTEGER", nullable: false),
                CreatedAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: false),
                StartedAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true),
                CompletedAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true),
                Message = table.Column<string>(type: "TEXT", nullable: true)
            },
            constraints: table =>
            {
                table.PrimaryKey("PK_BackupTasks", x => x.Id);
            });

        migrationBuilder.CreateTable(
            name: "ProviderConfigs",
            columns: table => new
            {
                Id = table.Column<int>(type: "INTEGER", nullable: false)
                    .Annotation("Sqlite:Autoincrement", true),
                Provider = table.Column<string>(type: "TEXT", nullable: false),
                BaseUrl = table.Column<string>(type: "TEXT", nullable: false),
                Username = table.Column<string>(type: "TEXT", nullable: false),
                ApiToken = table.Column<string>(type: "TEXT", nullable: false),
                IsConfigured = table.Column<bool>(type: "INTEGER", nullable: false),
                IsVerified = table.Column<bool>(type: "INTEGER", nullable: false),
                VerifiedAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true),
                LastSyncAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true)
            },
            constraints: table =>
            {
                table.PrimaryKey("PK_ProviderConfigs", x => x.Id);
            });

        migrationBuilder.CreateTable(
            name: "Repositories",
            columns: table => new
            {
                Id = table.Column<int>(type: "INTEGER", nullable: false)
                    .Annotation("Sqlite:Autoincrement", true),
                Provider = table.Column<string>(type: "TEXT", nullable: false),
                ExternalId = table.Column<string>(type: "TEXT", nullable: false),
                Name = table.Column<string>(type: "TEXT", nullable: false),
                FullName = table.Column<string>(type: "TEXT", nullable: false),
                CloneUrl = table.Column<string>(type: "TEXT", nullable: false),
                DefaultBranch = table.Column<string>(type: "TEXT", nullable: true),
                LastBackupStatus = table.Column<string>(type: "TEXT", nullable: false),
                LastBackupAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true),
                LastBackupMessage = table.Column<string>(type: "TEXT", nullable: true),
                LastBackupSizeBytes = table.Column<long>(type: "INTEGER", nullable: true)
            },
            constraints: table =>
            {
                table.PrimaryKey("PK_Repositories", x => x.Id);
            });

        migrationBuilder.CreateTable(
            name: "S3Configs",
            columns: table => new
            {
                Id = table.Column<int>(type: "INTEGER", nullable: false)
                    .Annotation("Sqlite:Autoincrement", true),
                Endpoint = table.Column<string>(type: "TEXT", nullable: false),
                Region = table.Column<string>(type: "TEXT", nullable: false),
                Bucket = table.Column<string>(type: "TEXT", nullable: false),
                AccessKeyId = table.Column<string>(type: "TEXT", nullable: false),
                SecretAccessKey = table.Column<string>(type: "TEXT", nullable: false),
                IsVerified = table.Column<bool>(type: "INTEGER", nullable: false),
                VerifiedAt = table.Column<DateTimeOffset>(type: "TEXT", nullable: true)
            },
            constraints: table =>
            {
                table.PrimaryKey("PK_S3Configs", x => x.Id);
            });

        migrationBuilder.CreateIndex(
            name: "IX_ProviderConfigs_Provider",
            table: "ProviderConfigs",
            column: "Provider",
            unique: true);

        migrationBuilder.CreateIndex(
            name: "IX_Repositories_Provider_ExternalId",
            table: "Repositories",
            columns: new[] { "Provider", "ExternalId" },
            unique: true);
    }

    protected override void Down(MigrationBuilder migrationBuilder)
    {
        migrationBuilder.DropTable(name: "BackupTasks");
        migrationBuilder.DropTable(name: "ProviderConfigs");
        migrationBuilder.DropTable(name: "Repositories");
        migrationBuilder.DropTable(name: "S3Configs");
    }
}
