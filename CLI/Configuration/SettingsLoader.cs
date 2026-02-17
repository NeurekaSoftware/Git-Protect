using CLI.Configuration.Models;
using CLI.Runtime;
using CLI.Services.Scheduling;
using YamlDotNet.Core;
using YamlDotNet.Serialization;
using YamlDotNet.Serialization.NamingConventions;

namespace CLI.Configuration;

public sealed class SettingsLoader
{
    private static readonly HashSet<string> SupportedBackupProviders = new(StringComparer.OrdinalIgnoreCase)
    {
        "github",
        "gitlab",
        "forgejo"
    };

    private readonly IDeserializer _deserializer;

    public SettingsLoader()
    {
        _deserializer = new DeserializerBuilder()
            .WithNamingConvention(CamelCaseNamingConvention.Instance)
            .IgnoreUnmatchedProperties()
            .Build();
    }

    public SettingsLoadResult Load(string path)
    {
        if (string.IsNullOrWhiteSpace(path))
        {
            return SettingsLoadResult.Failure(["settings path is required."]);
        }

        if (!File.Exists(path))
        {
            return SettingsLoadResult.Failure([$"settings file not found: '{path}'"]);
        }

        Settings settings;
        try
        {
            var yaml = File.ReadAllText(path);
            settings = _deserializer.Deserialize<Settings>(yaml) ?? new Settings();
        }
        catch (YamlException exception)
        {
            return SettingsLoadResult.Failure([$"YAML parse error in '{path}': {exception.Message}"]);
        }
        catch (Exception exception)
        {
            return SettingsLoadResult.Failure([$"failed to load settings '{path}': {exception.Message}"]);
        }

        Normalize(settings);
        var errors = Validate(settings);

        return errors.Count == 0
            ? SettingsLoadResult.Success(settings)
            : SettingsLoadResult.Failure(errors);
    }

    private static void Normalize(Settings settings)
    {
        settings.Logging ??= new LoggingConfig();
        settings.Logging.LogLevel = NormalizeLogLevel(settings.Logging.LogLevel);
        settings.Storage ??= new StorageConfig();
        settings.Storage.ForcePathStyle ??= false;
        settings.Storage.ArchiveMode ??= true;
        settings.Storage.PruneOrphanedMirrors ??= false;
        settings.Credentials ??= new Dictionary<string, CredentialConfig>(StringComparer.OrdinalIgnoreCase);
        settings.Credentials = new Dictionary<string, CredentialConfig>(settings.Credentials, StringComparer.OrdinalIgnoreCase);
        settings.Backups ??= [];
        settings.Mirrors ??= [];
        settings.Schedule ??= new ScheduleConfig();
        settings.Schedule.Backups ??= new JobScheduleConfig();
        settings.Schedule.Mirrors ??= new JobScheduleConfig();

        foreach (var backup in settings.Backups)
        {
            if (backup is null)
            {
                continue;
            }

            backup.Enabled ??= true;
            backup.Lfs ??= false;
            backup.Provider = backup.Provider?.Trim().ToLowerInvariant();
        }

        foreach (var mirror in settings.Mirrors)
        {
            if (mirror is null)
            {
                continue;
            }

            mirror.Enabled ??= true;
            mirror.Lfs ??= false;
            mirror.Force ??= false;
        }
    }

    private static List<string> Validate(Settings settings)
    {
        var errors = new List<string>();
        ValidateLogging(settings, errors);
        ValidateStorage(settings, errors);
        ValidateBackups(settings, errors);
        ValidateMirrors(settings, errors);
        ValidateSchedule(settings, errors);
        return errors;
    }

    private static void ValidateLogging(Settings settings, List<string> errors)
    {
        if (settings.Logging is null)
        {
            errors.Add("logging is required.");
            return;
        }

        if (!AppLogger.TryParseLogLevel(settings.Logging.LogLevel, out _))
        {
            var supported = string.Join(", ", AppLogger.SupportedLogLevels);
            errors.Add($"logging.logLevel '{settings.Logging.LogLevel}' is invalid. Supported values: {supported}.");
        }
    }

    private static void ValidateStorage(Settings settings, List<string> errors)
    {
        if (settings.Storage is null)
        {
            errors.Add("storage is required.");
            return;
        }

        if (string.IsNullOrWhiteSpace(settings.Storage.Endpoint))
        {
            errors.Add("storage.endpoint is required.");
        }
        else if (!IsValidHttpUrl(settings.Storage.Endpoint))
        {
            errors.Add("storage.endpoint must be an absolute http or https URL.");
        }

        if (string.IsNullOrWhiteSpace(settings.Storage.Region))
        {
            errors.Add("storage.region is required.");
        }

        if (string.IsNullOrWhiteSpace(settings.Storage.AccessKeyId))
        {
            errors.Add("storage.accessKeyId is required.");
        }

        if (string.IsNullOrWhiteSpace(settings.Storage.SecretAccessKey))
        {
            errors.Add("storage.secretAccessKey is required.");
        }

        if (string.IsNullOrWhiteSpace(settings.Storage.Bucket))
        {
            errors.Add("storage.bucket is required.");
        }
    }

    private static void ValidateBackups(Settings settings, List<string> errors)
    {
        for (var i = 0; i < settings.Backups.Count; i++)
        {
            var backup = settings.Backups[i];
            if (backup is null)
            {
                errors.Add($"backups[{i}] is required.");
                continue;
            }

            if (string.IsNullOrWhiteSpace(backup.Provider))
            {
                errors.Add($"backups[{i}].provider is required.");
            }
            else if (!SupportedBackupProviders.Contains(backup.Provider))
            {
                errors.Add($"backups[{i}].provider '{backup.Provider}' is not supported. Supported values: github, gitlab, forgejo.");
            }

            if (string.IsNullOrWhiteSpace(backup.Credential))
            {
                errors.Add($"backups[{i}].credential is required.");
                continue;
            }

            if (!settings.Credentials.ContainsKey(backup.Credential))
            {
                errors.Add($"backups[{i}].credential references unknown credential '{backup.Credential}'.");
            }

            if (string.IsNullOrWhiteSpace(backup.BaseUrl))
            {
                continue;
            }

            if (!IsValidHttpUrl(backup.BaseUrl))
            {
                errors.Add($"backups[{i}].baseUrl must be an absolute http or https URL.");
            }
        }
    }

    private static void ValidateMirrors(Settings settings, List<string> errors)
    {
        for (var i = 0; i < settings.Mirrors.Count; i++)
        {
            var mirror = settings.Mirrors[i];
            if (mirror is null)
            {
                errors.Add($"mirrors[{i}] is required.");
                continue;
            }

            if (string.IsNullOrWhiteSpace(mirror.Url))
            {
                errors.Add($"mirrors[{i}].url is required.");
            }
            else if (!IsValidHttpUrl(mirror.Url))
            {
                errors.Add($"mirrors[{i}].url must be an absolute http or https URL.");
            }

            if (string.IsNullOrWhiteSpace(mirror.Credential))
            {
                continue;
            }

            if (!settings.Credentials.ContainsKey(mirror.Credential))
            {
                errors.Add($"mirrors[{i}].credential references unknown credential '{mirror.Credential}'.");
            }
        }
    }

    private static void ValidateSchedule(Settings settings, List<string> errors)
    {
        ValidateCron(settings.Schedule.Backups.Cron, "schedule.backups.cron", errors);
        ValidateCron(settings.Schedule.Mirrors.Cron, "schedule.mirrors.cron", errors);
    }

    private static void ValidateCron(string? cronExpression, string path, List<string> errors)
    {
        if (string.IsNullOrWhiteSpace(cronExpression))
        {
            errors.Add($"{path} is required.");
            return;
        }

        if (!CronScheduleParser.TryParse(cronExpression, out _, out var error))
        {
            errors.Add($"{path} is invalid: {error}");
        }
    }

    private static bool IsValidHttpUrl(string url)
    {
        if (!Uri.TryCreate(url, UriKind.Absolute, out var uri))
        {
            return false;
        }

        return uri.Scheme.Equals(Uri.UriSchemeHttp, StringComparison.OrdinalIgnoreCase) ||
               uri.Scheme.Equals(Uri.UriSchemeHttps, StringComparison.OrdinalIgnoreCase);
    }

    private static string NormalizeLogLevel(string? value)
    {
        var normalized = value?.Trim().ToLowerInvariant();
        return string.IsNullOrWhiteSpace(normalized)
            ? AppLogger.DefaultLogLevel
            : normalized;
    }
}
