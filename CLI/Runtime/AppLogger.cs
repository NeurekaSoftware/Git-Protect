namespace CLI.Runtime;

public enum AppLogLevel
{
    Debug = 0,
    Info = 1,
    Warn = 2,
    Error = 3
}

public static class AppLogger
{
    private static readonly object Sync = new();
    private static AppLogLevel _minimumLevel = AppLogLevel.Info;

    public const string DefaultLogLevel = "info";

    public static IReadOnlyList<string> SupportedLogLevels => ["debug", "info", "warn", "error"];

    public static bool TryParseLogLevel(string? value, out AppLogLevel level)
    {
        switch (value?.Trim().ToLowerInvariant())
        {
            case "debug":
                level = AppLogLevel.Debug;
                return true;
            case "info":
                level = AppLogLevel.Info;
                return true;
            case "warn":
                level = AppLogLevel.Warn;
                return true;
            case "error":
                level = AppLogLevel.Error;
                return true;
            default:
                level = AppLogLevel.Info;
                return false;
        }
    }

    public static string ToConfigValue(AppLogLevel level)
    {
        return level switch
        {
            AppLogLevel.Debug => "debug",
            AppLogLevel.Info => "info",
            AppLogLevel.Warn => "warn",
            _ => "error"
        };
    }

    public static void SetMinimumLevel(AppLogLevel level)
    {
        lock (Sync)
        {
            _minimumLevel = level;
        }
    }

    public static AppLogLevel GetMinimumLevel()
    {
        lock (Sync)
        {
            return _minimumLevel;
        }
    }

    public static void Debug(string message) => Write(AppLogLevel.Debug, message);

    public static void Info(string message) => Write(AppLogLevel.Info, message);

    public static void Warn(string message) => Write(AppLogLevel.Warn, message);

    public static void Error(string message, Exception? exception = null)
    {
        Write(AppLogLevel.Error, message);

        if (exception is not null && GetMinimumLevel() <= AppLogLevel.Debug)
        {
            Write(AppLogLevel.Debug, exception.ToString());
        }
    }

    private static void Write(AppLogLevel level, string message)
    {
        if (!ShouldLog(level))
        {
            return;
        }

        var line = $"[{ToLevelLabel(level)}]: {message}";

        if (level == AppLogLevel.Error)
        {
            Console.Error.WriteLine(line);
            return;
        }

        Console.WriteLine(line);
    }

    private static bool ShouldLog(AppLogLevel level)
    {
        lock (Sync)
        {
            return level >= _minimumLevel;
        }
    }

    private static string ToLevelLabel(AppLogLevel level)
    {
        return level switch
        {
            AppLogLevel.Debug => "DEBUG",
            AppLogLevel.Info => "INFO",
            AppLogLevel.Warn => "WARN",
            _ => "ERROR"
        };
    }
}
