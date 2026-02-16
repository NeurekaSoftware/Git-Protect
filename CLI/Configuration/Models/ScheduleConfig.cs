using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class ScheduleConfig
{
    [YamlMember(Alias = "backups")]
    public JobScheduleConfig Backups { get; set; } = new();

    [YamlMember(Alias = "mirrors")]
    public JobScheduleConfig Mirrors { get; set; } = new();
}
