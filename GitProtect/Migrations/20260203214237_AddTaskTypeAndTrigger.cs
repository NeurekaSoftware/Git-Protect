using Microsoft.EntityFrameworkCore.Migrations;

#nullable disable

namespace GitProtect.Migrations
{
    /// <inheritdoc />
    public partial class AddTaskTypeAndTrigger : Migration
    {
        /// <inheritdoc />
        protected override void Up(MigrationBuilder migrationBuilder)
        {
            migrationBuilder.AlterColumn<string>(
                name: "Provider",
                table: "BackupTasks",
                type: "TEXT",
                nullable: true,
                oldClrType: typeof(string),
                oldType: "TEXT");

            migrationBuilder.AddColumn<string>(
                name: "TaskType",
                table: "BackupTasks",
                type: "TEXT",
                nullable: false,
                defaultValue: "Backup");

            migrationBuilder.AddColumn<string>(
                name: "Trigger",
                table: "BackupTasks",
                type: "TEXT",
                nullable: false,
                defaultValue: "Manual");
        }

        /// <inheritdoc />
        protected override void Down(MigrationBuilder migrationBuilder)
        {
            migrationBuilder.DropColumn(
                name: "TaskType",
                table: "BackupTasks");

            migrationBuilder.DropColumn(
                name: "Trigger",
                table: "BackupTasks");

            migrationBuilder.AlterColumn<string>(
                name: "Provider",
                table: "BackupTasks",
                type: "TEXT",
                nullable: false,
                defaultValue: "",
                oldClrType: typeof(string),
                oldType: "TEXT",
                oldNullable: true);
        }
    }
}
