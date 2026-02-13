using Coravel;
using GitProtect.Client.Pages;
using GitProtect.Components;
using GitProtect.Data;
using GitProtect.Endpoints;
using GitProtect.Services;
using Microsoft.EntityFrameworkCore;
using MudBlazor.Services;
using System.Data;

var builder = WebApplication.CreateBuilder(args);

// Allow Docker-prefixed env vars (GITPROTECT__*) to override appsettings.
builder.Configuration.AddEnvironmentVariables("GITPROTECT__");

var sqliteConnectionString = ResolveSqliteConnectionString(builder.Configuration, builder.Environment);
if (IsInMemorySqlite(sqliteConnectionString))
{
    throw new InvalidOperationException(
        "In-memory SQLite is not supported for GitProtect runtime. Configure ConnectionStrings:GitProtectDb to a file-based database.");
}

builder.Services.AddRazorComponents()
    .AddInteractiveWebAssemblyComponents();
builder.Services.AddMudServices();

var apiBaseUri = ResolveApiBaseUri(builder.Configuration);

builder.Services.AddHttpClient();
builder.Services.AddHttpClient<GitProtect.Client.Services.ApiClient>(client =>
{
    if (apiBaseUri is not null)
    {
        client.BaseAddress = apiBaseUri;
    }
});
builder.Services.AddDbContext<GitProtectDbContext>(options =>
    options.UseSqlite(sqliteConnectionString));

builder.Services.AddScoped<ProviderApiService>();
builder.Services.AddScoped<S3StorageService>();
builder.Services.AddScoped<GitMirrorService>();
builder.Services.AddScoped<BackupService>();
builder.Services.AddSingleton<BackupQueue>();
builder.Services.AddHostedService<BackupWorker>();
builder.Services.AddScoped<BackupScheduleInvoker>();
builder.Services.AddScoped<RetentionPolicyInvoker>();
builder.Services.AddScheduler();

var app = builder.Build();

if (app.Environment.IsDevelopment())
{
    app.UseWebAssemblyDebugging();
}
else
{
    app.UseExceptionHandler("/Error", createScopeForErrors: true);
    app.UseHsts();
}
app.UseStatusCodePagesWithReExecute("/not-found", createScopeForStatusCodePages: true);
app.UseHttpsRedirection();

app.UseAntiforgery();

app.MapStaticAssets();
app.MapGitProtectApi();
app.MapRazorComponents<App>()
    .AddInteractiveWebAssemblyRenderMode()
    .AddAdditionalAssemblies(typeof(GitProtect.Client._Imports).Assembly);

using (var scope = app.Services.CreateScope())
{
    var db = scope.ServiceProvider.GetRequiredService<GitProtectDbContext>();
    db.Database.Migrate();
    EnsureSqliteLegacySchema(db);
}

using (var verificationScope = app.Services.CreateScope())
{
    var db = verificationScope.ServiceProvider.GetRequiredService<GitProtectDbContext>();
    _ = db.Repositories.Any();
}

app.Services.UseScheduler(scheduler =>
{
    scheduler.Schedule<BackupScheduleInvoker>()
        .EveryMinute()
        .PreventOverlapping("backup-schedule");

    scheduler.Schedule<RetentionPolicyInvoker>()
        .Hourly()
        .PreventOverlapping("retention-policy");
});

app.Run();

static Uri? ResolveApiBaseUri(IConfiguration configuration)
{
    var explicitBase = configuration["ApiBaseUrl"];
    if (Uri.TryCreate(explicitBase, UriKind.Absolute, out var explicitUri) && explicitUri.Port != 0)
    {
        return NormalizeListenAddress(explicitUri);
    }

    var urls = configuration["urls"] ?? configuration["ASPNETCORE_URLS"];
    if (string.IsNullOrWhiteSpace(urls))
    {
        return null;
    }

    var first = urls
        .Split(';', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
        .FirstOrDefault();
    if (string.IsNullOrWhiteSpace(first))
    {
        return null;
    }

    var candidate = first
        .Replace("http://+", "http://127.0.0.1", StringComparison.OrdinalIgnoreCase)
        .Replace("https://+", "https://127.0.0.1", StringComparison.OrdinalIgnoreCase)
        .Replace("http://*", "http://127.0.0.1", StringComparison.OrdinalIgnoreCase)
        .Replace("https://*", "https://127.0.0.1", StringComparison.OrdinalIgnoreCase);

    if (!Uri.TryCreate(candidate, UriKind.Absolute, out var parsed) || parsed.Port == 0)
    {
        return null;
    }

    return NormalizeListenAddress(parsed);
}

static Uri NormalizeListenAddress(Uri uri)
{
    var builder = new UriBuilder(uri);
    if (string.Equals(builder.Host, "0.0.0.0", StringComparison.OrdinalIgnoreCase)
        || string.Equals(builder.Host, "::", StringComparison.OrdinalIgnoreCase))
    {
        builder.Host = "127.0.0.1";
    }

    return builder.Uri;
}

static string ResolveSqliteConnectionString(IConfiguration configuration, IWebHostEnvironment environment)
{
    var configured = configuration.GetConnectionString("GitProtectDb");
    if (!string.IsNullOrWhiteSpace(configured))
    {
        return configured;
    }

    var dataDirectory = Directory.Exists("/data")
        ? "/data"
        : environment.ContentRootPath;

    var databasePath = Path.Combine(dataDirectory, "gitprotect.db");
    return $"Data Source={databasePath}";
}

static bool IsInMemorySqlite(string connectionString)
{
    return connectionString.Contains(":memory:", StringComparison.OrdinalIgnoreCase)
        || connectionString.Contains("mode=memory", StringComparison.OrdinalIgnoreCase);
}

static void EnsureSqliteLegacySchema(GitProtectDbContext db)
{
    if (!db.Database.IsSqlite())
    {
        return;
    }

    var connection = db.Database.GetDbConnection();
    if (connection.State != ConnectionState.Open)
    {
        connection.Open();
    }

    bool hasUsePathStyleColumn;
    using (var tableInfoCommand = connection.CreateCommand())
    {
        tableInfoCommand.CommandText = "PRAGMA table_info('S3Configs');";
        using var reader = tableInfoCommand.ExecuteReader();
        hasUsePathStyleColumn = false;

        while (reader.Read())
        {
            var columnName = reader.GetString(1);
            if (string.Equals(columnName, "UsePathStyle", StringComparison.OrdinalIgnoreCase))
            {
                hasUsePathStyleColumn = true;
                break;
            }
        }
    }

    if (hasUsePathStyleColumn)
    {
        return;
    }

    using var addColumnCommand = connection.CreateCommand();
    addColumnCommand.CommandText = "ALTER TABLE \"S3Configs\" ADD COLUMN \"UsePathStyle\" INTEGER NOT NULL DEFAULT 0;";
    addColumnCommand.ExecuteNonQuery();
}
