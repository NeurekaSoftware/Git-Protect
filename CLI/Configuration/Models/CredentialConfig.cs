using YamlDotNet.Serialization;

namespace CLI.Configuration.Models;

public sealed class CredentialConfig
{
    [YamlMember(Alias = "username")]
    public string? Username { get; set; }

    [YamlMember(Alias = "apiKey")]
    public string? ApiKey { get; set; }
}
