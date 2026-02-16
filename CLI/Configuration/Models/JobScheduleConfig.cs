using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class JobScheduleConfig
{
    [YamlMember(Alias = "cron")]
    public string? Cron { get; set; }
}
