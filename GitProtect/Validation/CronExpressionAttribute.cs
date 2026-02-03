using System.ComponentModel.DataAnnotations;

namespace GitProtect.Validation;

[AttributeUsage(AttributeTargets.Property | AttributeTargets.Field | AttributeTargets.Parameter)]
public sealed class CronExpressionAttribute : ValidationAttribute
{
    public CronExpressionAttribute()
    {
        ErrorMessage = "Cron expression is invalid.";
    }

    protected override ValidationResult? IsValid(object? value, ValidationContext validationContext)
    {
        var expression = value as string;
        if (!CronExpressionValidator.TryValidate(expression, out var error))
        {
            return new ValidationResult(error ?? ErrorMessage);
        }

        return ValidationResult.Success;
    }
}
