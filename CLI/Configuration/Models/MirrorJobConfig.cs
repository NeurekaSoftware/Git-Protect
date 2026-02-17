using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class MirrorJobConfig
{
    [YamlMember(Alias = "url")]
    public string? Url { get; set; }

    [YamlMember(Alias = "credential")]
    public string? Credential { get; set; }

    [YamlMember(Alias = "lfs")]
    public bool? Lfs { get; set; }

    [YamlMember(Alias = "enabled")]
    public bool? Enabled { get; set; }
}
