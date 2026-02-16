using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class LoggingConfig
{
    [YamlMember(Alias = "logLevel")]
    public string? LogLevel { get; set; }
}
