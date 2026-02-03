namespace GitProtect.Models;

public sealed class ProviderConfig
{
    public int Id { get; set; }
    public ProviderType Provider { get; set; }
    public string BaseUrl { get; set; } = string.Empty;
    public string Username { get; set; } = string.Empty;
    public string ApiToken { get; set; } = string.Empty;
    public bool IsConfigured { get; set; }
    public bool IsVerified { get; set; }
    public DateTimeOffset? VerifiedAt { get; set; }
    public DateTimeOffset? LastSyncAt { get; set; }
}
