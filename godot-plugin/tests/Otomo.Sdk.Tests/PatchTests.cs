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
using System.Threading.Tasks;
using OtomoSdk;
using OtomoSdk.Patch;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class PatchTests : IDisposable
{
    private readonly string _dir;

    public PatchTests()
    {
        _dir = Path.Combine(Path.GetTempPath(), "otomo-patch-" + Guid.NewGuid().ToString("N"));
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

    private string PatchDir => Path.Combine(_dir, "patch");
    private string ManifestPath => Path.Combine(PatchDir, "manifest.json");
    private string EtagPath => Path.Combine(PatchDir, "etag.txt");
    private string BlobsDir => Path.Combine(PatchDir, "blobs");

    private string BlobPath(string sha256) => Path.Combine(BlobsDir, sha256);
    private string PartPath(string sha256) => Path.Combine(BlobsDir, sha256 + ".part");

    private OtomoConfig Config(string clientVersion = "1.0.0", Action<string>? log = null, TimeSpan? stallTimeout = null) =>
        new("https://example.test", _dir)
        {
            Channel = "live",
            ClientVersion = clientVersion,
            RequestTimeout = TimeSpan.FromSeconds(5),
            StallTimeout = stallTimeout ?? TimeSpan.FromSeconds(20),
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

    private static byte[] Corrupt(byte[] source)
    {
        var copy = (byte[])source.Clone();
        for (var i = 0; i < copy.Length; i++)
            copy[i] ^= 0xFF;
        return copy;
    }

    /// Delivers exactly `first`, then never completes another read on its own -- only
    /// cancellation (the stall timer under test) ends it. Used to prove StallTimeout fires
    /// when a connection stays open but stops delivering bytes.
    private sealed class StallingStream : Stream
    {
        private readonly byte[] _first;
        private bool _sentFirst;

        public StallingStream(byte[] first) => _first = first;

        public override bool CanRead => true;
        public override bool CanSeek => false;
        public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException();
        public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }

        public override async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken ct)
        {
            if (!_sentFirst)
            {
                _sentFirst = true;
                var n = Math.Min(count, _first.Length);
                Array.Copy(_first, 0, buffer, offset, n);
                return n;
            }

            await Task.Delay(Timeout.InfiniteTimeSpan, ct);
            return 0;
        }

        public override void Flush() { }
        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException();
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
        public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }

    private sealed class StallingContent : HttpContent
    {
        private readonly byte[] _first;

        public StallingContent(byte[] first) => _first = first;

        protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context) =>
            throw new NotSupportedException("stalling content is read via CreateContentReadStreamAsync");

        protected override bool TryComputeLength(out long length)
        {
            length = 0;
            return false;
        }

        protected override Task<Stream> CreateContentReadStreamAsync() =>
            Task.FromResult<Stream>(new StallingStream(_first));
    }

    // ---- Tests ---------------------------------------------------------------

    [Fact]
    public async Task FreshInstall_DownloadsVerifiesLoadsAndMounts()
    {
        var (cfgSha, cfgBytes) = Blob("{\"x\":1}");
        var (packSha, packBytes) = Blob("pack-contents");
        var manifest = new Manifest
        {
            Format = 1,
            Channel = "live",
            ReleaseId = 3,
            MinClientVersion = "0.0.0",
            Config = { ["game"] = Cfg(cfgSha, cfgBytes.Length) },
            Packs = { Pack("base", packSha, packBytes.Length) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e1\""));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]>
        {
            [cfgSha] = cfgBytes,
            [packSha] = packBytes,
        });

        var remote = new RemoteConfig();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        var result = await client.RunAsync();

        Assert.Equal(PatchResult.Ready, result);
        Assert.Equal(PatchState.Ready, client.State);
        Assert.NotNull(client.Current);
        Assert.Equal(3, client.Current!.ReleaseId);
        Assert.True(File.Exists(BlobPath(cfgSha)));
        Assert.True(File.Exists(BlobPath(packSha)));
        Assert.True(File.Exists(ManifestPath));
        Assert.Equal("\"e1\"", File.ReadAllText(EtagPath));
        Assert.Single(mounter.Mounts);
        Assert.Equal(packSha, mounter.Mounts[0].Pack.Sha256);
        Assert.Equal(BlobPath(packSha), mounter.Mounts[0].OsPath);
        Assert.True(remote.TryGet("game", "x", out var value));
        Assert.Equal(1, value.GetInt32());
    }

    [Fact]
    public async Task SecondRun_NotModified_ReusesCacheAndSendsEtag()
    {
        var (packSha, packBytes) = Blob("pack-v1");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 1,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };

        var first = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e-saved\""));
        first.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [packSha] = packBytes });
        var client = new PatchClient(new HttpClient(first), Config(), new RemoteConfig(), new RecordingMounter());
        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        var second = new FakeHandler().Respond(_ => NotModified());
        var remote2 = new RemoteConfig();
        var mounter2 = new RecordingMounter();
        var client2 = new PatchClient(new HttpClient(second), Config(), remote2, mounter2);

        Assert.Equal(PatchResult.Ready, await client2.RunAsync());

        var manifestRequest = Assert.Single(second.Requests);
        Assert.Equal("\"e-saved\"", manifestRequest.Headers["If-None-Match"]);
        Assert.DoesNotContain(second.Requests, r => r.RequestUri!.AbsolutePath.Contains("/blob/"));
        Assert.Single(mounter2.Mounts);
        Assert.Equal(packSha, mounter2.Mounts[0].Pack.Sha256);
        Assert.NotNull(client2.Current);
    }

    [Fact]
    public async Task Resume_UsesRangeFromExistingPart()
    {
        var bytes = Encoding.UTF8.GetBytes("0123456789abcdef");
        var sha = Sha256(bytes);
        var half = bytes.Length / 2;
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 1,
            Packs = { Pack("base", sha, bytes.Length) },
        };

        Directory.CreateDirectory(BlobsDir);
        File.WriteAllBytes(PartPath(sha), bytes[..half]);

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e1\""));
        handler.FallbackResponder = request =>
        {
            var range = request.Headers.TryGetValues("Range", out var values) ? values.First() : null;
            Assert.Equal($"bytes={half}-", range);
            return BlobResponse(bytes[half..], HttpStatusCode.PartialContent);
        };

        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.Equal(bytes, File.ReadAllBytes(BlobPath(sha)));
        Assert.False(File.Exists(PartPath(sha)));
    }

    [Fact]
    public async Task CorruptBlob_RetriesOnceAndSucceeds()
    {
        var (sha, bytes) = Blob("good");
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", sha, bytes.Length) },
        };

        var handler = new FakeHandler()
            .Respond(_ => ManifestResponse(manifest, "\"e1\""))
            .Respond(_ => BlobResponse(Corrupt(bytes)))
            .Respond(_ => BlobResponse(bytes));

        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.Equal(bytes, File.ReadAllBytes(BlobPath(sha)));
    }

    [Fact]
    public async Task CorruptBlobTwice_GoesOfflineWithoutSavingManifest()
    {
        var (sha, bytes) = Blob("good");
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", sha, bytes.Length) },
        };

        var handler = new FakeHandler()
            .Respond(_ => ManifestResponse(manifest, "\"e1\""))
            .Respond(_ => BlobResponse(Corrupt(bytes)))
            .Respond(_ => BlobResponse(Corrupt(bytes)));

        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.False(File.Exists(ManifestPath));
        Assert.False(File.Exists(BlobPath(sha)));
        Assert.Empty(mounter.Mounts);
        Assert.Equal("hash_mismatch", client.LastError!.Code);
    }

    [Fact]
    public async Task MinClientVersion_On200And304_ReturnsClientTooOld()
    {
        var manifest = new Manifest { Format = 1, MinClientVersion = "0.0.0" };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e1\"", minVersion: "9.0.0"));
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config("1.0.0"), new RemoteConfig(), mounter);

        Assert.Equal(PatchResult.ClientTooOld, await client.RunAsync());
        Assert.Equal(PatchState.ClientTooOld, client.State);
        Assert.Equal("9.0.0", client.RequiredClientVersion);
        Assert.Empty(mounter.Mounts);

        var (sha, bytes) = Blob("cached");
        var cached = new Manifest { Format = 1, Packs = { Pack("base", sha, bytes.Length) } };
        SeedCache(cached, "\"old\"", (sha, bytes));

        var handler304 = new FakeHandler().Respond(_ => NotModified("9.0.0"));
        var mounter304 = new RecordingMounter();
        var client304 = new PatchClient(new HttpClient(handler304), Config("1.0.0"), new RemoteConfig(), mounter304);

        Assert.Equal(PatchResult.ClientTooOld, await client304.RunAsync());
        Assert.Equal("9.0.0", client304.RequiredClientVersion);
        Assert.Empty(mounter304.Mounts);
    }

    [Fact]
    public async Task ServerUnreachable_WithGoodCache_UsesCache()
    {
        var (cfgSha, cfgBytes) = Blob("{\"x\":1}");
        var (packSha, packBytes) = Blob("pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 7,
            Config = { ["game"] = Cfg(cfgSha, cfgBytes.Length) },
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        SeedCache(cached, "\"old\"", (cfgSha, cfgBytes), (packSha, packBytes));

        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("down"));
        var remote = new RemoteConfig();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.Equal(PatchState.OfflineReady, client.State);
        Assert.NotNull(client.Current);
        Assert.Equal(7, client.Current!.ReleaseId);
        Assert.Single(mounter.Mounts);
        Assert.Equal(packSha, mounter.Mounts[0].Pack.Sha256);
        Assert.True(remote.TryGet("game", "x", out var value));
        Assert.Equal(1, value.GetInt32());
    }

    [Fact]
    public async Task ServerUnreachable_NoCache_GoesOfflineEmpty()
    {
        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("down"));
        var remote = new RemoteConfig();
        remote.Load("stale", "{\"x\":1}");
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.Equal(PatchState.OfflineReady, client.State);
        Assert.Null(client.Current);
        Assert.Empty(mounter.Mounts);
        Assert.Empty(remote.Names);
    }

    [Fact]
    public async Task NewRelease_DroppedPack_OldBlobDeleted()
    {
        var (aSha, aBytes) = Blob("A");
        var (bSha, bBytes) = Blob("B");
        var release1 = new Manifest
        {
            Format = 1,
            ReleaseId = 1,
            Packs = { Pack("a", aSha, aBytes.Length), Pack("b", bSha, bBytes.Length) },
        };

        var first = new FakeHandler().Respond(_ => ManifestResponse(release1, "\"e1\""));
        first.FallbackResponder = BlobRouter(new Dictionary<string, byte[]>
        {
            [aSha] = aBytes,
            [bSha] = bBytes,
        });
        var client = new PatchClient(new HttpClient(first), Config(), new RemoteConfig(), new RecordingMounter());
        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.True(File.Exists(BlobPath(bSha)));

        var release2 = new Manifest
        {
            Format = 1,
            ReleaseId = 2,
            Packs = { Pack("a", aSha, aBytes.Length) },
        };
        var second = new FakeHandler().Respond(_ => ManifestResponse(release2, "\"e2\""));
        second.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [aSha] = aBytes });
        var client2 = new PatchClient(new HttpClient(second), Config(), new RemoteConfig(), new RecordingMounter());

        Assert.Equal(PatchResult.Ready, await client2.RunAsync());
        Assert.True(File.Exists(BlobPath(aSha)));
        Assert.False(File.Exists(BlobPath(bSha)));
        Assert.DoesNotContain(second.Requests, r => r.RequestUri!.AbsolutePath.Contains("/blob/"));
    }

    [Fact]
    public async Task PathTrickSha_Rejected_NoFileWrittenOutsideDataDir()
    {
        var manifest = new Manifest
        {
            Format = 1,
            Config = { ["evil"] = Cfg("../../evil", 5) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e1\""));
        var remote = new RemoteConfig();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.Empty(mounter.Mounts);
        Assert.DoesNotContain(handler.Requests, r => r.RequestUri!.AbsolutePath.Contains("/blob/"));
        Assert.False(File.Exists(Path.Combine(_dir, "evil")));
        Assert.False(File.Exists(Path.Combine(Directory.GetParent(_dir)!.FullName, "evil")));
    }

    [Fact]
    public async Task MountFailure_GoesOfflineAndDoesNotSaveManifest()
    {
        var (packSha, packBytes) = Blob("pack");
        var manifest = new Manifest
        {
            Format = 1,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };

        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest, "\"e1\""));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [packSha] = packBytes });
        var mounter = new RecordingMounter { Result = false };
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.False(File.Exists(ManifestPath));
        Assert.Null(client.Current);
        Assert.Single(mounter.Mounts);
    }

    [Fact]
    public async Task UnsupportedFormat_UsesCachedManifest()
    {
        var (cfgSha, cfgBytes) = Blob("{\"x\":9}");
        var (packSha, packBytes) = Blob("cached-pack");
        var cached = new Manifest
        {
            Format = 1,
            ReleaseId = 5,
            Config = { ["game"] = Cfg(cfgSha, cfgBytes.Length) },
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        SeedCache(cached, "\"e-old\"", (cfgSha, cfgBytes), (packSha, packBytes));

        var bad = new Manifest { Format = 2, ReleaseId = 6 };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(bad, "\"e-new\""));
        var remote = new RemoteConfig();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), remote, mounter);

        Assert.Equal(PatchResult.OfflineReady, await client.RunAsync());
        Assert.NotNull(client.Current);
        Assert.Equal(5, client.Current!.ReleaseId);
        Assert.Single(mounter.Mounts);
        Assert.True(remote.TryGet("game", "x", out var value));
        Assert.Equal(9, value.GetInt32());
    }

    [Fact]
    public async Task OversizedBlob_IsDiscarded_AndGoesOffline()
    {
        var (packSha, packBytes) = Blob("pack-contents");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 4,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var oversized = packBytes.Concat(new byte[1024 * 1024]).ToArray();
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = BlobRouter(new Dictionary<string, byte[]> { [packSha] = oversized });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        var result = await client.RunAsync();

        Assert.Equal(PatchResult.OfflineReady, result);
        Assert.Equal("size_mismatch", client.LastError?.Code);
        Assert.False(File.Exists(PartPath(packSha)));
        Assert.False(File.Exists(BlobPath(packSha)));
        Assert.False(File.Exists(ManifestPath));
    }

    [Fact]
    public async Task StalledBlob_TimesOutAndGoesOffline()
    {
        var (packSha, packBytes) = Blob("pack-contents-longer-than-one-chunk");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 4,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var handler = new FakeHandler().Respond(_ => ManifestResponse(manifest));
        handler.FallbackResponder = _ =>
            new HttpResponseMessage(HttpStatusCode.OK) { Content = new StallingContent(packBytes) };
        var client = new PatchClient(
            new HttpClient(handler),
            Config(stallTimeout: TimeSpan.FromMilliseconds(50)),
            new RemoteConfig(),
            new RecordingMounter());

        var result = await client.RunAsync();

        Assert.Equal(PatchResult.OfflineReady, result);
        Assert.Equal("stalled", client.LastError?.Code);
        Assert.False(File.Exists(BlobPath(packSha)));
    }

    [Fact]
    public async Task SecondRunInSameProcess_DoesNotMountThePackAgain()
    {
        var (packSha, packBytes) = Blob("pack-contents");
        var manifest = new Manifest
        {
            Format = 1,
            ReleaseId = 5,
            Packs = { Pack("base", packSha, packBytes.Length) },
        };
        var blobs = BlobRouter(new Dictionary<string, byte[]> { [packSha] = packBytes });
        var manifestCalls = 0;
        var handler = new FakeHandler();
        handler.FallbackResponder = request =>
            !request.RequestUri!.AbsolutePath.EndsWith("/manifest", StringComparison.Ordinal) ? blobs(request)
            : manifestCalls++ == 0 ? ManifestResponse(manifest, "\"e5\"") : NotModified();
        var mounter = new RecordingMounter();
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), mounter);

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.Equal(PatchResult.Ready, await client.RunAsync());

        Assert.Single(mounter.Mounts);
    }

    [Fact]
    public async Task NullCollectionsInManifest_AreEmpty()
    {
        var handler = new FakeHandler().Respond(_ => new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new StringContent("{\"format\":1,\"channel\":\"live\",\"release_id\":6,\"config\":null,\"packs\":null}",
                Encoding.UTF8, "application/json"),
        });
        var client = new PatchClient(new HttpClient(handler), Config(), new RemoteConfig(), new RecordingMounter());

        Assert.Equal(PatchResult.Ready, await client.RunAsync());
        Assert.Equal(6, client.Current!.ReleaseId);
    }
}
