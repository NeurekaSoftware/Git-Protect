namespace GitProtect.Models;

public sealed class S3Config
{
    public int Id { get; set; }
    public string Endpoint { get; set; } = string.Empty;
    public string Region { get; set; } = string.Empty;
    public string Bucket { get; set; } = string.Empty;
    public string AccessKeyId { get; set; } = string.Empty;
    public string SecretAccessKey { get; set; } = string.Empty;
    public bool UsePathStyle { get; set; }
    public bool IsVerified { get; set; }
    public DateTime? VerifiedAt { get; set; }
}
