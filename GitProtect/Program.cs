using Coravel;
using GitProtect.Client.Pages;
using GitProtect.Components;
using GitProtect.Data;
using GitProtect.Endpoints;
using GitProtect.Services;
using Microsoft.EntityFrameworkCore;

var builder = WebApplication.CreateBuilder(args);

// Allow Docker-prefixed env vars (GITPROTECT__*) to override appsettings.
builder.Configuration.AddEnvironmentVariables("GITPROTECT__");

builder.Services.AddRazorComponents()
    .AddInteractiveWebAssemblyComponents();

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
    options.UseSqlite(builder.Configuration.GetConnectionString("GitProtectDb")));

builder.Services.AddScoped<ProviderApiService>();
builder.Services.AddScoped<S3StorageService>();
builder.Services.AddScoped<GitMirrorService>();
builder.Services.AddScoped<BackupService>();
builder.Services.AddSingleton<BackupQueue>();
builder.Services.AddHostedService<BackupWorker>();
builder.Services.AddScoped<BackupScheduleInvoker>();
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

app.Services.UseScheduler(scheduler =>
{
    scheduler.Schedule<BackupScheduleInvoker>()
        .EveryMinute()
        .PreventOverlapping("backup-schedule");
});

using (var scope = app.Services.CreateScope())
{
    var db = scope.ServiceProvider.GetRequiredService<GitProtectDbContext>();
    db.Database.Migrate();
}

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
