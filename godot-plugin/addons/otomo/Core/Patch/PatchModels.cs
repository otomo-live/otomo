#nullable enable
using System;
using System.Collections.Generic;
using OtomoSdk.Http;

namespace OtomoSdk.Patch;

/// One file (config document or pack) that still has to be downloaded. <see cref="AlreadyHave"/>
/// is the number of bytes already present in an existing ".part" file.
public sealed record PlannedFile(
    string Kind,
    string Name,
    string Sha256,
    long Size,
    long AlreadyHave);

/// Immutable description of what a run would download, raised through <see cref="PatchClient.Planned"/>
/// after the manifest is validated and before the first byte is fetched.
public sealed class PatchPlan
{
    public long ReleaseId { get; }
    public IReadOnlyList<PlannedFile> Files { get; }
    public long TotalBytes { get; }
    public long AlreadyHaveBytes { get; }
    public long BytesToDownload { get; }
    public int FileCount { get; }

    public bool IsUpToDate => FileCount == 0;

    public PatchPlan(long releaseId, IReadOnlyList<PlannedFile> files)
    {
        if (files is null)
            throw new ArgumentNullException(nameof(files));

        ReleaseId = releaseId;

        var copy = new List<PlannedFile>(files);
        Files = copy.AsReadOnly();

        long total = 0;
        long have = 0;
        foreach (var file in copy)
        {
            total += file.Size;
            have += file.AlreadyHave;
        }

        TotalBytes = total;
        AlreadyHaveBytes = have;
        BytesToDownload = total - have;
        FileCount = copy.Count;
    }
}

/// Snapshot of download progress. Safe to read from any thread through <see cref="PatchClient.CurrentProgress"/>.
public readonly record struct PatchProgress(
    long BytesDone,
    long BytesTotal,
    int FilesDone,
    int FilesTotal,
    string CurrentFile,
    double BytesPerSecond,
    double? EtaSeconds);

/// Raised exactly once at the end of every <see cref="PatchClient.RunAsync"/> that was not cancelled.
public sealed record PatchSummary(
    PatchResult Result,
    long? ReleaseId,
    long BytesDownloaded,
    int FilesDownloaded,
    TimeSpan Duration,
    OtomoError? Error);
