using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class BackupJobConfig
{
    [YamlMember(Alias = "provider")]
    public string? Provider { get; set; }

    [YamlMember(Alias = "credential")]
    public string? Credential { get; set; }

    [YamlMember(Alias = "baseUrl")]
    public string? BaseUrl { get; set; }

    [YamlMember(Alias = "lfs")]
    public bool? Lfs { get; set; }

    [YamlMember(Alias = "enabled")]
    public bool? Enabled { get; set; }
}
