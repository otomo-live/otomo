#nullable enable
using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk;
using OtomoSdk.Patch;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class PatchHooksTests : IDisposable
{
    private readonly string _dir;

    public PatchHooksTests()
    {
        _dir = Path.Combine(Path.GetTempPath(), "otomo-hooks-" + Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(_dir);
    }

    public void Dispose()
    {
        try
        {
            Directory.Delete(_dir, recursive: true);
        }
        catch (IOException)
        {
        }
        catch (UnauthorizedAccessException)
        {
        }
    }

    // ---- Fixture -------------------------------------------------------------

    private sealed class RecordingMounter : IPackMounter
    {
        public List<(string OsPath, ManifestPack Pack)> Mounts { get; } = new();
        public bool Result { get; set; } = true;

        public bool Mount(string osPath, ManifestPack pack)
        {
            Mounts.Add((osPath, pack));
            return Result;
        }
    }

    /// Manual clock so throttling, speed and duration are fully deterministic.
    private sealed class FakeTimeProvider : TimeProvider
    {
        private long _timestamp;
        private DateTimeOffset _utc = DateTimeOffset.UnixEpoch;

        public override long TimestampFrequency => TimeSpan.TicksPerSecond;

        public override long GetTimestamp() => _timestamp;

        public override DateTimeOffset GetUtcNow() => _utc;

        public void Advance(TimeSpan delta)
        {
            _timestamp += (long)(delta.TotalSeconds * TimestampFrequency);
            _utc += delta;
        }
    }

    /// Serves the blob bytes while advancing the fake clock on every read.
    private sealed class ClockStream : Stream
    {
        private readonly byte[] _data;
        private readonly FakeTimeProvider _clock;
        private readonly TimeSpan _step;
        private int _position;

        public ClockStream(byte[] data, FakeTimeProvider clock, TimeSpan step)
        {
            _data = data;
            _clock = clock;
            _step = step;
        }

        public override bool CanRead => true;
        public override bool CanSeek => false;
        public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException();

        public override long Position
        {
            get => _position;
            set => throw new NotSupportedException();
        }

        public override int Read(byte[] buffer, int offset, int count)
        {
            var read = Math.Min(count, _data.Length - _position);
            Array.Copy(_data, _position, buffer, offset, read);
            _position += read;
            _clock.Advance(_step);
            return read;
        }

        public override ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            var read = Math.Min(buffer.Length, _data.Length - _position);
            _data.AsMemory(_position, read).CopyTo(buffer);
            _position += read;
            _clock.Advance(_step);
            return new ValueTask<int>(read);
        }

        public override void Flush()
        {
        }

        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
        public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }

    private string PatchDir => Path.Combine(_dir, "patch");
    private string ManifestPath => Path.Combine(PatchDir, "manifest.json");
    private string EtagPath => Path.Combine(PatchDir, "etag.txt");
    private string BlobsDir => Path.Combine(PatchDir, "blobs");

    private string BlobPath(string sha256) => Path.Combine(BlobsDir, sha256);
    private string PartPath(string sha256) => Path.Combine(BlobsDir, sha256 + ".part");

    private OtomoConfig Config(string clientVersion = "1.0.0", Action<string>? log = null) =>
        new("https://example.test", _dir)
        {
            Channel = "live",
            ClientVersion = clientVersion,
            RequestTimeout = TimeSpan.FromSeconds(5),
            Log = log,
        };

    private static (string Sha, byte[] Bytes) Blob(string content)
    {
        var bytes = Encoding.UTF8.GetBytes(content);
        return (Sha256(bytes), bytes);
    }

    private static string Sha256(byte[] bytes) =>
        Convert.ToHexString(SHA256.HashData(bytes)).ToLowerInvariant();

    private static ManifestFile Cfg(string sha256, long size) => new() { Sha256 = sha256, Size = size };

    private static ManifestPack Pack(string name, string sha256, long size) =>
        new() { Name = name, Sha256 = sha256, Size = size };

    private static HttpResponseMessage ManifestResponse(
        Manifest manifest,
        string etag = "\"etag-1\"",
        string? minVersion = null)
    {
        var response = new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new StringContent(JsonSerializer.Serialize(manifest), Encoding.UTF8, "application/json"),
        };
        response.Headers.ETag = new EntityTagHeaderValue(etag);
        if (minVersion is not null)
            response.Headers.TryAddWithoutValidation("X-Min-Client-Version", minVersion);
        return response;
    }

    private static HttpResponseMessage BlobResponse(byte[] bytes, HttpStatusCode status = HttpStatusCode.OK) =>
        new(status) { Content = new ByteArrayContent(bytes) };

    private static HttpResponseMessage NotModified(string? minVersion = null)
    {
        var response = new HttpResponseMessage(HttpStatusCode.NotModified);
        if (minVersion is not null)
            response.Headers.TryAddWithoutValidation("X-Min-Client-Version", minVersion);
        return response;
    }

    private static Func<HttpRequestMessage, HttpResponseMessage> BlobRouter(Dictionary<string, byte[]> blobs) =>
        request =>
        {
            var path = request.RequestUri!.AbsolutePath;
            foreach (var pair in blobs)
                if (path.EndsWith("/blob/" + pair.Key, StringComparison.Ordinal))
                    return BlobResponse(pair.Value);

            throw new InvalidOperationException($"unexpected request {path}");
        };

    private void SeedCache(Manifest manifest, string etag, params (string Sha, byte[] Bytes)[] blobs)
    {
        Directory.CreateDirectory(BlobsDir);
        File.WriteAllText(ManifestPath, JsonSerializer.Serialize(manifest));
        File.WriteAllText(EtagPath, etag);
        foreach (var (sha, bytes) in blobs)
            File.WriteAllBytes(BlobPath(sha), bytes);
    }

    private static bool HasBlobRequest(FakeHandler handler) =>
        handler.Requests.Any(r => r.RequestUri!.AbsolutePath.Contains("/blob/", StringComparison.Ordinal));

    // ---- Plan ----------------------------------------------------------------

    [Fact]
    public async Task Plan_FreshInstall_ListsConfigAndPack()
    {
        var (cfgSha, cfgBytes) = Blob("{\"x\":1}");
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            Channel = "live",
            ReleaseId = 11,
            Config = { ["game"] = Cfg(cfgSha, cfgBytes.Length) },
            Packs = { Pack("base", packSha, packBytes.Length) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]>
        {
            [cfgSha] = cfgBytes,
            [packSha] = packBytes,
        });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        PatchPlan? plan = null;
        client.Planned += p => plan = p;

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.NotNull(plan);
        Assert.Equal(11, plan!.ReleaseId);
        Assert.Equal(2, plan.FileCount);
        Assert.False(plan.IsUpToDate);
        Assert.Equal(cfgBytes.Length + packBytes.Length, plan.TotalBytes);
        Assert.Equal(0, plan.AlreadyHaveBytes);
        Assert.Equal(plan.TotalBytes, plan.BytesToDownload);
        Assert.Contains(plan.Files, f => f.Kind == "config" && f.Name == "game" && f.Sha256 == cfgSha);
        Assert.Contains(plan.Files, f => f.Kind == "pack" && f.Name == "base" && f.Sha256 == packSha);
    }

    [Fact]
    public async Task Plan_ExistingPart_CountsResumeBytes()
    {
        var (packSha, packBytes) = Blob("pack-contents");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 2,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var half = packBytes.Length / 2;

        Directory.CreateDirectory(BlobsDir);
        File.WriteAllBytes(PartPath(packSha), packBytes[..half]);

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = request =>
        {
            var range = request.Headers.TryGetValues("Range", out var values) ? values.First() : null;
            Assert.Equal($"bytes={half}-", range);
            return BlobResponse(packBytes[half..], HttpStatusCode.PartialContent);
        };

        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        PatchPlan? plan = null;
        client.Planned += p => plan = p;

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.NotNull(plan);
        Assert.Equal(half, plan!.AlreadyHaveBytes);
        Assert.Equal(packBytes.Length, plan.TotalBytes);
        Assert.Equal(packBytes.Length - half, plan.BytesToDownload);
    }

    [Fact]
    public async Task Plan_UpToDate_IsEmpty()
    {
        var (packSha, packBytes) = Blob("pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 5,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        SeedCache(cached, "\"e\"", (packSha, packBytes));

        var handler = new FakeHandler().Respond(_ => NotModified());
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        PatchPlan? plan = null;
        client.Planned += p => plan = p;

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.NotNull(plan);
        Assert.True(plan!.IsUpToDate);
        Assert.Equal(0, plan.FileCount);
        Assert.Empty(plan.Files);
    }

    [Fact]
    public async Task Plan_NotRaised_ForClientTooOld()
    {
        var manifest = new Manifest { Format = 1, MinClientVersion = "0.0.0" };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, minVersion: "9.0.0"));
        var client = new PatchClient(new HttpClient(handler), Config("1.0.0"), new RemoteConfig(), new RecordingMounter());

        var plans = 0;
        client.Planned += _ => plans++;

        Assert.Equal(PatchResult.ClientTooOld, await client.RunAsync());
        Assert.Equal(0, plans);
    }

    [Fact]
    public async Task Plan_NotRaised_WhenOffline()
    {
        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("down"));
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        var plans = 0;
        client.Planned += _ => plans++;

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.Equal(0, plans);
    }

    // ---- Confirmation --------------------------------------------------------

    [Fact]
    public async Task Confirm_False_DeclinesAndUsesCachedRelease()
    {
        var (oldSha, oldBytes) = Blob("old-pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 7,
            Packs = { Pack("base", oldSha, oldBytes.Length) },
        };
        SeedCache(cached, "\"old\"", (oldSha, oldBytes));

        var (newSha, newBytes) = Blob("new-pack");
        var target = new Manifest
        {
            Format = 1,
            ReleaseId = 8,
            Packs = { Pack("base", newSha, newBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(target, "\"new\""));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [newSha] = newBytes });

        var remote = new RemoteConfig();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        var asked = 0;
        client.ConfirmDownload = (_, _) =>
        {
            asked++;
            return Task.FromResult(false);
        };

        Assert.Equal(PatchResult.Declined, await client.RunAsync());

        Assert.Equal(1, asked);
        Assert.Equal("download_declined", client.LastError!.Code);
        Assert.False(HasBlobRequest(handler));
        Assert.NotNull(client.Current);
        Assert.Equal(7, client.Current!.ReleaseId);
        Assert.Single(mounter.Mounts);
        Assert.Equal(oldSha, mounter.Mounts[0].Pack.Sha256);
    }

    [Fact]
    public async Task Confirm_True_DownloadsThePlan()
    {
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 3,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [packSha] = packBytes });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        PatchPlan? seen = null;
        client.ConfirmDownload = (plan, _) =>
        {
            seen = plan;
            return Task.FromResult(true);
        };

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.NotNull(seen);
        Assert.Equal(1, seen!.FileCount);
        Assert.True(File.Exists(BlobPath(packSha)));
    }

    [Fact]
    public async Task Confirm_Throwing_IsTreatedAsDeclined()
    {
        var (oldSha, oldBytes) = Blob("old-pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 7,
            Packs = { Pack("base", oldSha, oldBytes.Length) },
        };
        SeedCache(cached, "\"old\"", (oldSha, oldBytes));

        var (newSha, newBytes) = Blob("new-pack");
        var target = new Manifest
        {
            Format = 1,
            ReleaseId = 8,
            Packs = { Pack("base", newSha, newBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(target, "\"new\""));

        var logs = new List<string>();
        var client = new PatchClient(new HttpClient(handler), Config(log: logs.Add), new RemoteConfig(), new RecordingMounter());
        client.ConfirmDownload = (_, _) => throw new InvalidOperationException("boom");

        Assert.Equal(PatchResult.Declined, await client.RunAsync());
        Assert.Equal("download_declined", client.LastError!.Code);
        Assert.Contains(logs, l => l.Contains("confirmation", StringComparison.OrdinalIgnoreCase));
    }

    [Fact]
    public async Task Confirm_NoHook_DownloadsWithoutAsking()
    {
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [packSha] = packBytes });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.True(File.Exists(BlobPath(packSha)));
    }

    [Fact]
    public async Task Confirm_EmptyPlan_IsNeverAsked()
    {
        var (packSha, packBytes) = Blob("pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 5,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        SeedCache(cached, "\"e\"", (packSha, packBytes));

        var handler = new FakeHandler().Respond(_ => NotModified());
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        var asked = 0;
        client.ConfirmDownload = (_, _) =>
        {
            asked++;
            return Task.FromResult(true);
        };

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.Equal(0, asked);
    }

    // ---- Progress ------------------------------------------------------------

    [Fact]
    public async Task Progress_IsThrottled_AndEndsAtOneHundredPercent()
    {
        var clock = new FakeTimeProvider();
        var bytes = new byte[1024 * 1024];
        new Random(7).NextBytes(bytes);
        var sha = Sha256(bytes);
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 1,
            Packs = { Pack("base", sha, bytes.Length) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = _ => new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new StreamContent(new ClockStream(bytes, clock, TimeSpan.FromMilliseconds(1))),
        };

        var client = new PatchClient(
            new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter(), clock)
        {
            ProgressInterval = TimeSpan.FromMilliseconds(100),
        };

        var events = new List<PatchProgress>();
        client.ProgressChanged += p => events.Add(p);

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.NotEmpty(events);
        // 16 chunk reads in 16 ms: only the forced (start, file done, final) events must fire.
        Assert.True(events.Count <= 5, $"expected a handful of events, got {events.Count}");

        var last = events[^1];
        Assert.Equal(bytes.Length, last.BytesDone);
        Assert.Equal(bytes.Length, last.BytesTotal);
        Assert.Equal(1, last.FilesDone);
        Assert.Equal(1, last.FilesTotal);
        Assert.Equal("base", last.CurrentFile);
        Assert.Equal(last, client.CurrentProgress);
    }

    [Fact]
    public async Task Progress_SpeedAndEta_ComeFromTheClock()
    {
        var clock = new FakeTimeProvider();
        var bytes = new byte[1024 * 1024];
        new Random(9).NextBytes(bytes);
        var sha = Sha256(bytes);
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", sha, bytes.Length) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = _ => new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new StreamContent(new ClockStream(bytes, clock, TimeSpan.FromMilliseconds(10))),
        };

        var client = new PatchClient(
            new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter(), clock);

        var events = new List<PatchProgress>();
        client.ProgressChanged += p => events.Add(p);

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        var latest = events.Last(e => e.BytesDone == bytes.Length);
        Assert.True(latest.BytesPerSecond > 0, $"speed={latest.BytesPerSecond}");
        Assert.NotNull(latest.EtaSeconds);
    }

    // ---- Finished ------------------------------------------------------------

    [Fact]
    public async Task Finished_Ready_ReportsDownloads()
    {
        var (cfgSha, cfgBytes) = Blob("{\"x\":1}");
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 3,
            Config = { ["game"] = Cfg(cfgSha, cfgBytes.Length) },
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]>
        {
            [cfgSha] = cfgBytes,
            [packSha] = packBytes,
        });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        var summaries = new List<PatchSummary>();
        client.Finished += s => summaries.Add(s);

        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        var summary = Assert.Single(summaries);
        Assert.Equal(PatchResult.Ready, summary.Result);
        Assert.Equal(3, summary.ReleaseId);
        Assert.Equal(cfgBytes.Length + packBytes.Length, summary.BytesDownloaded);
        Assert.Equal(2, summary.FilesDownloaded);
        Assert.Null(summary.Error);
        Assert.True(summary.Duration >= TimeSpan.Zero);
    }

    [Fact]
    public async Task Finished_OfflineReady_IsRaisedOnce()
    {
        var (packSha, packBytes) = Blob("pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 7,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        SeedCache(cached, "\"old\"", (packSha, packBytes));

        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("down"));
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        var summaries = new List<PatchSummary>();
        client.Finished += s => summaries.Add(s);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());

        var summary = Assert.Single(summaries);
        Assert.Equal(PatchResult.OfflineReady, summary.Result);
        Assert.Equal(7, summary.ReleaseId);
        Assert.Equal(0, summary.BytesDownloaded);
        Assert.Equal(0, summary.FilesDownloaded);
    }

    [Fact]
    public async Task Finished_ClientTooOld_IsRaised()
    {
        var manifest = new Manifest { Format = 1, MinClientVersion = "0.0.0" };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, minVersion: "9.0.0"));
        var client = new PatchClient(new HttpClient(handler), Config("1.0.0"), new RemoteConfig(), new RecordingMounter());

        var summaries = new List<PatchSummary>();
        client.Finished += s => summaries.Add(s);

        Assert.Equal(PatchResult.ClientTooOld, await client.RunAsync());

        var summary = Assert.Single(summaries);
        Assert.Equal(PatchResult.ClientTooOld, summary.Result);
        Assert.Null(summary.ReleaseId);
        Assert.NotNull(summary.Error);
        Assert.Equal("client_too_old", summary.Error!.Code);
    }

    [Fact]
    public async Task Finished_Declined_IsRaised()
    {
        var (oldSha, oldBytes) = Blob("old-pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 7,
            Packs = { Pack("base", oldSha, oldBytes.Length) },
        };
        SeedCache(cached, "\"old\"", (oldSha, oldBytes));

        var (newSha, newBytes) = Blob("new-pack");
        var target = new Manifest
        {
            Format = 1,
            ReleaseId = 8,
            Packs = { Pack("base", newSha, newBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(target, "\"new\""));
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());
        client.ConfirmDownload = (_, _) => Task.FromResult(false);

        var summaries = new List<PatchSummary>();
        client.Finished += s => summaries.Add(s);

        Assert.Equal(PatchResult.Declined, await client.RunAsync());

        var summary = Assert.Single(summaries);
        Assert.Equal(PatchResult.Declined, summary.Result);
        Assert.Equal(0, summary.BytesDownloaded);
        Assert.Equal(0, summary.FilesDownloaded);
        Assert.Equal("download_declined", summary.Error!.Code);
    }

    [Fact]
    public async Task Finished_NotRaised_WhenCallerCancels()
    {
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        using var cts = new CancellationTokenSource();
        client.ConfirmDownload = (_, ct) =>
        {
            cts.Cancel();
            ct.ThrowIfCancellationRequested();
            return Task.FromResult(true);
        };

        var summaries = new List<PatchSummary>();
        client.Finished += s => summaries.Add(s);

        await Assert.ThrowsAnyAsync<OperationCanceledException>(() => client.RunAsync(cts.Token));
        Assert.Empty(summaries);
    }
}
