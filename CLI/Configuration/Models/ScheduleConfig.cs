using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class ScheduleConfig
{
    [YamlMember(Alias = "repositories")]
    public JobScheduleConfig Repositories { get; set; } = new();
}
