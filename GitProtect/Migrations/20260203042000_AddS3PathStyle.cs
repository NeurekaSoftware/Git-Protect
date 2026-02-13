using GitProtect.Data;
using Microsoft.EntityFrameworkCore.Infrastructure;
using Microsoft.EntityFrameworkCore.Migrations;

#nullable disable

namespace GitProtect.Migrations;

[DbContext(typeof(GitProtectDbContext))]
[Migration("20260203042000_AddS3PathStyle")]
public partial class AddS3PathStyle : Migration
{
    protected override void Up(MigrationBuilder migrationBuilder)
    {
        migrationBuilder.AddColumn<bool>(
            name: "UsePathStyle",
            table: "S3Configs",
            type: "INTEGER",
            nullable: false,
            defaultValue: false);
    }

    protected override void Down(MigrationBuilder migrationBuilder)
    {
        migrationBuilder.DropColumn(
            name: "UsePathStyle",
            table: "S3Configs");
    }
}
