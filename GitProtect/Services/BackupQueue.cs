using System.Threading.Channels;
using GitProtect.Models;

namespace GitProtect.Services;

public sealed class BackupQueue
{
    private readonly Channel<BackupJob> _queue = Channel.CreateUnbounded<BackupJob>();

    public ValueTask EnqueueAsync(BackupJob job) => _queue.Writer.WriteAsync(job);

    public ValueTask<BackupJob> DequeueAsync(CancellationToken cancellationToken) => _queue.Reader.ReadAsync(cancellationToken);
}

public sealed record BackupJob(int TaskId, ProviderType Provider, int? RepositoryId);
