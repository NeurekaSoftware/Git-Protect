using CLI.Configuration;
using CLI.Runtime;
using CLI.Services.Backup;
using CLI.Services.Git;
using CLI.Services.Providers;
using CLI.Services.Scheduling;
using CLI.Services.Storage;

namespace CLI;

class Program
{
    private const string ContainerBinPath = "/app/bin";
    private const string ContainerDataPath = "/app/data";

    static async Task<int> Main(string[] args)
    {
        BuildMetadata.LoadFromEnvironment();
        Console.WriteLine($"Version: {BuildMetadata.Version}");
        Console.WriteLine($"Commit: {BuildMetadata.Commit}");

        var settingsPath = ResolveSettingsPath(args);
        var settingsLoader = new SettingsLoader();
        var settingsLoadResult = settingsLoader.Load(settingsPath);

        if (!settingsLoadResult.IsSuccess)
        {
            foreach (var error in settingsLoadResult.Errors)
            {
                Console.Error.WriteLine(error);
            }

            return 1;
        }

        var settings = settingsLoadResult.Settings!;
        using var liveSettings = new LiveSettings(settingsPath, settings, settingsLoader);
        liveSettings.Start();

        Console.WriteLine($"Loaded settings from '{settingsPath}'.");
        Console.WriteLine($"Watching settings file '{liveSettings.SettingsPath}' for changes.");
        Console.WriteLine($"Configured backups: {settings.Backups.Count}");
        Console.WriteLine($"Configured mirrors: {settings.Mirrors.Count}");

        var workingRoot = ResolveWorkingRoot();
        Directory.CreateDirectory(workingRoot);

        Func<CLI.Configuration.Models.StorageConfig, IObjectStorageService> objectStorageFactory =
            storage => new SimpleS3ObjectStorageService(storage);
        var gitRepositoryService = new GitCliRepositoryService();
        var providerFactory = new BackupProviderClientFactory(
        [
            new GitHubBackupProviderClient(),
            new GitLabBackupProviderClient(),
            new ForgejoBackupProviderClient()
        ]);

        var mirrorService = new MirrorService(gitRepositoryService, objectStorageFactory, workingRoot);
        var backupService = new BackupService(providerFactory, gitRepositoryService, objectStorageFactory, workingRoot);
        var retentionService = new RetentionService(objectStorageFactory);
        var scheduledJobRunner = new ScheduledJobRunner(() => liveSettings.Current, mirrorService, backupService, retentionService);

        using var shutdown = new CancellationTokenSource();
        Console.CancelKeyPress += (_, eventArgs) =>
        {
            eventArgs.Cancel = true;
            shutdown.Cancel();
        };

        Console.WriteLine("Scheduler started. Press Ctrl+C to stop.");

        try
        {
            await scheduledJobRunner.RunForeverAsync(shutdown.Token);
        }
        catch (OperationCanceledException) when (shutdown.IsCancellationRequested)
        {
            // Graceful shutdown.
        }

        Console.WriteLine("Scheduler stopped.");
        return 0;
    }

    private static string ResolveSettingsPath(string[] args)
    {
        if (args.Length > 0 && !string.IsNullOrWhiteSpace(args[0]))
        {
            return args[0];
        }

        var defaultPathCandidates = GetDefaultSettingsPathCandidates();

        foreach (var candidate in defaultPathCandidates)
        {
            if (File.Exists(candidate))
            {
                return candidate;
            }
        }

        return defaultPathCandidates[0];
    }

    private static string ResolveWorkingRoot()
    {
        return Path.Combine(Path.GetTempPath(), ".git-protect");
    }

    private static string[] GetDefaultSettingsPathCandidates()
    {
        if (IsRunningInContainer())
        {
            return
            [
                Path.Combine(ContainerBinPath, "settings.yaml"),
                Path.Combine(ContainerDataPath, "settings.yaml"),
                Path.Combine(Environment.CurrentDirectory, "settings.yaml")
            ];
        }

        return
        [
            Path.Combine(Environment.CurrentDirectory, "settings.yaml"),
            Path.Combine(Environment.CurrentDirectory, "CLI", "settings.yaml")
        ];
    }

    private static bool IsRunningInContainer()
    {
        var dotnetContainer = Environment.GetEnvironmentVariable("DOTNET_RUNNING_IN_CONTAINER");
        if (string.Equals(dotnetContainer, "true", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        return File.Exists("/.dockerenv");
    }
}
