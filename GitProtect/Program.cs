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

builder.Services.AddHttpClient();
builder.Services.AddScoped<GitProtect.Client.Services.ApiClient>();
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
