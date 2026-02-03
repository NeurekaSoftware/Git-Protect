using System.Globalization;

namespace GitProtect.Validation;

public static class CronExpressionValidator
{
    public const string DefaultDailyExpression = "0 0 * * *";

    public static bool TryValidate(string? expression, out string? error)
    {
        if (string.IsNullOrWhiteSpace(expression))
        {
            error = "Cron expression is required.";
            return false;
        }

        var parts = expression.Trim().Split(' ', StringSplitOptions.RemoveEmptyEntries);
        if (parts.Length != 5)
        {
            error = "Cron expression must have five parts (minute hour day month weekday).";
            return false;
        }

        if (!IsValidPart(parts[0], 0, 59, out error)
            || !IsValidPart(parts[1], 0, 23, out error)
            || !IsValidPart(parts[2], 1, 31, out error)
            || !IsValidPart(parts[3], 1, 12, out error)
            || !IsValidPart(parts[4], 0, 6, out error))
        {
            return false;
        }

        error = null;
        return true;
    }

    public static bool IsDue(string expression, DateTimeOffset now)
    {
        if (!TryValidate(expression, out _))
        {
            return false;
        }

        var parts = expression.Trim().Split(' ', StringSplitOptions.RemoveEmptyEntries);
        return MatchesPart(parts[0], now.Minute, 0, 59)
            && MatchesPart(parts[1], now.Hour, 0, 23)
            && MatchesPart(parts[2], now.Day, 1, 31)
            && MatchesPart(parts[3], now.Month, 1, 12)
            && MatchesPart(parts[4], (int)now.DayOfWeek, 0, 6);
    }

    private static bool IsValidPart(string part, int min, int max, out string? error)
    {
        error = null;
        if (part == "*")
        {
            return true;
        }

        if (part.StartsWith("*/", StringComparison.Ordinal))
        {
            var stepText = part[2..];
            if (!int.TryParse(stepText, NumberStyles.None, CultureInfo.InvariantCulture, out var step) || step <= 0)
            {
                error = "Cron expression has an invalid step value.";
                return false;
            }

            return true;
        }

        if (part.Contains('-'))
        {
            var rangeParts = part.Split('-', StringSplitOptions.RemoveEmptyEntries);
            if (rangeParts.Length != 2
                || !TryParseValue(rangeParts[0], min, max, out var start)
                || !TryParseValue(rangeParts[1], min, max, out var end)
                || start > end)
            {
                error = "Cron expression has an invalid range.";
                return false;
            }

            return true;
        }

        if (part.Contains(','))
        {
            var values = part.Split(',', StringSplitOptions.RemoveEmptyEntries);
            if (values.Length == 0)
            {
                error = "Cron expression has an invalid list.";
                return false;
            }

            foreach (var value in values)
            {
                if (!TryParseValue(value, min, max, out _))
                {
                    error = "Cron expression has an invalid list value.";
                    return false;
                }
            }

            return true;
        }

        if (!TryParseValue(part, min, max, out _))
        {
            error = "Cron expression has an invalid value.";
            return false;
        }

        return true;
    }

    private static bool MatchesPart(string part, int value, int min, int max)
    {
        if (part == "*")
        {
            return true;
        }

        if (part.StartsWith("*/", StringComparison.Ordinal))
        {
            if (!int.TryParse(part[2..], NumberStyles.None, CultureInfo.InvariantCulture, out var step) || step <= 0)
            {
                return false;
            }

            return (value - min) % step == 0;
        }

        if (part.Contains('-'))
        {
            var rangeParts = part.Split('-', StringSplitOptions.RemoveEmptyEntries);
            if (rangeParts.Length != 2
                || !TryParseValue(rangeParts[0], min, max, out var start)
                || !TryParseValue(rangeParts[1], min, max, out var end))
            {
                return false;
            }

            return value >= start && value <= end;
        }

        if (part.Contains(','))
        {
            var values = part.Split(',', StringSplitOptions.RemoveEmptyEntries);
            foreach (var valuePart in values)
            {
                if (TryParseValue(valuePart, min, max, out var parsed) && value == parsed)
                {
                    return true;
                }
            }

            return false;
        }

        return TryParseValue(part, min, max, out var exact) && value == exact;
    }

    private static bool TryParseValue(string value, int min, int max, out int parsed)
    {
        if (!int.TryParse(value, NumberStyles.None, CultureInfo.InvariantCulture, out parsed))
        {
            return false;
        }

        return parsed >= min && parsed <= max;
    }
}
