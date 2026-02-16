using CLI.Configuration.Models;
using YamlDotNet.Serialization;

namespace CLI.Configuration;

public sealed class Settings
{
    [YamlMember(Alias = "logging")]
    public LoggingConfig Logging { get; set; } = new();

    [YamlMember(Alias = "storage")]
    public StorageConfig Storage { get; set; } = new();

    [YamlMember(Alias = "credentials")]
    public Dictionary<string, CredentialConfig> Credentials { get; set; } = new(StringComparer.OrdinalIgnoreCase);

    [YamlMember(Alias = "backups")]
    public List<BackupJobConfig?> Backups { get; set; } = [];

    [YamlMember(Alias = "mirrors")]
    public List<MirrorJobConfig?> Mirrors { get; set; } = [];

    [YamlMember(Alias = "schedule")]
    public ScheduleConfig Schedule { get; set; } = new();
}
