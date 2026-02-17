namespace CLI.Services.Storage;

public sealed class ArchiveUploadResult
{
    public required string ObjectKey { get; init; }

    public required string Sha256 { get; init; }

    public bool Uploaded { get; init; }

    public bool ComparedWithHead { get; init; }
}
