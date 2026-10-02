#nullable enable
using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Http;

namespace OtomoSdk.Patch;

public enum PatchState
{
    Idle,
    Checking,
    Downloading,
    Verifying,
    Mounting,
    Ready,
    OfflineReady,
    ClientTooOld,
}

/// The updater. Talks to /patch/v1 with the raw <see cref="HttpClient"/> because it needs
/// ETag and Range handling, which the shared OtomoHttp transport does not expose.
public sealed class PatchClient : IPatchClient
{
    private const int MaxParallelDownloads = 2;
    private const int BufferSize = 64 * 1024;
    private static readonly TimeSpan SpeedWindow = TimeSpan.FromSeconds(2);

    private readonly HttpClient _http;
    private readonly OtomoConfig _config;
    private readonly RemoteConfig _remote;
    private readonly IPackMounter _mounter;
    private readonly TimeProvider _time;
    // Packs cannot be unmounted, so remember them for the whole process: a later RunAsync
    // (the in-menu re-check) must not mount the same pack twice.
    private readonly HashSet<string> _mounted = new(StringComparer.Ordinal);

    private long _progressDone;
    private long _progressTotal;
    private int _progressFilesDone;
    private int _progressFilesTotal;
    private volatile string _currentFile = "";
    private long _speedDownloaded;
    private readonly Queue<(long Timestamp, long Bytes)> _speedSamples = new();
    private readonly object _speedGate = new();
    private readonly object _throttleGate = new();
    private readonly object _progressGate = new();
    private long? _lastRaiseTimestamp;
    private PatchProgress _currentProgress;

    private long _runBytesDownloaded;
    private int _runFilesDownloaded;

    public PatchClient(
        HttpClient http,
        OtomoConfig config,
        RemoteConfig remoteConfig,
        IPackMounter mounter,
        TimeProvider? timeProvider = null)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _config = config ?? throw new ArgumentNullException(nameof(config));
        _remote = remoteConfig ?? throw new ArgumentNullException(nameof(remoteConfig));
        _mounter = mounter ?? throw new ArgumentNullException(nameof(mounter));
        _time = timeProvider ?? TimeProvider.System;
    }

    public PatchState State { get; private set; } = PatchState.Idle;
    public Manifest? Current { get; private set; }
    public string? RequiredClientVersion { get; private set; }
    public OtomoError? LastError { get; private set; }

    /// Latest progress snapshot; safe to read from any thread.
    public PatchProgress CurrentProgress
    {
        get
        {
            lock (_progressGate)
                return _currentProgress;
        }
    }

    /// Minimum time between two throttled <see cref="ProgressChanged"/> events.
    public TimeSpan ProgressInterval { get; set; } = TimeSpan.FromMilliseconds(100);

    /// Asked once per run when the plan has files. Returning false uses the cached release
    /// exactly like the offline path and reports <see cref="PatchResult.Declined"/>.
    public Func<PatchPlan, CancellationToken, Task<bool>>? ConfirmDownload { get; set; }

    public event Action<PatchState>? StateChanged;
    public event Action<long, long>? Progress;
    public event Action<PatchPlan>? Planned;
    public event Action<PatchProgress>? ProgressChanged;
    public event Action<PatchSummary>? Finished;

    private string PatchDir => Path.Combine(_config.DataDir, "patch");
    private string ManifestPath => Path.Combine(PatchDir, "manifest.json");
    private string EtagPath => Path.Combine(PatchDir, "etag.txt");
    private string BlobsDir => Path.Combine(PatchDir, "blobs");

    private string BlobPath(string sha256) => Path.Combine(BlobsDir, sha256);
    private string PartPath(string sha256) => Path.Combine(BlobsDir, sha256 + ".part");
    private string BlobUrl(string sha256) => $"{_config.BaseUrl}/patch/v1/blob/{sha256}";

    public async Task<PatchResult> RunAsync(CancellationToken ct = default)
    {
        var startTimestamp = _time.GetTimestamp();
        ResetRun();

        var result = await RunCoreAsync(ct);

        RaiseFinished(result, startTimestamp);
        return result;
    }

    private async Task<PatchResult> RunCoreAsync(CancellationToken ct)
    {
        SetState(PatchState.Checking);

        var (cached, cachedEtag) = await LoadCacheAsync(ct);

        var fetch = await FetchManifestAsync(cached, cachedEtag, ct);
        if (fetch.Outcome == FetchOutcome.TooOld)
        {
            RequiredClientVersion = fetch.Minimum;
            LastError = new OtomoError(
                0,
                "client_too_old",
                $"client {_config.ClientVersion} is older than required {fetch.Minimum}",
                "");
            _config.Log?.Invoke($"patch: client too old (have {_config.ClientVersion}, need {fetch.Minimum})");
            SetState(PatchState.ClientTooOld);
            return PatchResult.ClientTooOld;
        }

        if (fetch.Outcome == FetchOutcome.Offline)
            return await OfflineAsync(cached, ct);

        var target = fetch.Manifest!;
        var targetEtag = fetch.Etag;

        if (!ValidateManifest(target))
        {
            LastError ??= new OtomoError(0, "bad_manifest", "manifest contains an invalid sha256 or pack name", "");
            _config.Log?.Invoke("patch: manifest validation failed");
            return await OfflineAsync(cached, ct);
        }

        var plan = BuildPlan(target);
        Planned?.Invoke(plan);

        if (plan.FileCount > 0 && ConfirmDownload is not null && !await ConfirmAsync(plan, ct))
            return await DeclinedAsync(cached, ct);

        if (plan.FileCount > 0 && !await DownloadAllAsync(plan, ct))
            return await OfflineAsync(cached, ct);

        SetState(PatchState.Mounting);

        var docs = await LoadConfigDocsAsync(target, ct);
        if (docs is null)
        {
            LastError ??= new OtomoError(0, "missing_blob", "a config blob is missing", "");
            return await OfflineAsync(cached, ct);
        }

        _remote.ReplaceAll(docs);

        if (!MountPacks(target))
            return await OfflineAsync(cached, ct); // do NOT save the manifest

        SaveCache(target, targetEtag);
        CleanupBlobs(target);

        Current = target;
        SetState(PatchState.Ready);
        _config.Log?.Invoke($"patch: ready (release_id={target.ReleaseId})");
        return PatchResult.Ready;
    }

    // ---- Planning + confirmation -------------------------------------------------

    private PatchPlan BuildPlan(Manifest target)
    {
        var files = new List<PlannedFile>();

        foreach (var pair in target.Config)
        {
            var file = pair.Value;
            if (File.Exists(BlobPath(file.Sha256)))
                continue;
            files.Add(new PlannedFile("config", pair.Key, file.Sha256, file.Size, PartLength(file.Sha256)));
        }

        foreach (var pack in target.Packs)
        {
            if (File.Exists(BlobPath(pack.Sha256)))
                continue;
            files.Add(new PlannedFile("pack", pack.Name, pack.Sha256, pack.Size, PartLength(pack.Sha256)));
        }

        return new PatchPlan(target.ReleaseId, files);
    }

    private long PartLength(string sha256)
    {
        var part = PartPath(sha256);
        return File.Exists(part) ? new FileInfo(part).Length : 0;
    }

    private async Task<bool> ConfirmAsync(PatchPlan plan, CancellationToken ct)
    {
        var confirm = ConfirmDownload;
        if (confirm is null)
            return true;

        try
        {
            return await confirm(plan, ct);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception e)
        {
            _config.Log?.Invoke($"patch: download confirmation failed: {e.Message}");
            return false;
        }
    }

    private async Task<PatchResult> DeclinedAsync(Manifest? cached, CancellationToken ct)
    {
        _config.Log?.Invoke("patch: download declined; using cached release");
        await OfflineAsync(cached, ct);
        LastError = new OtomoError(0, "download_declined", "download declined by caller", "");
        return PatchResult.Declined;
    }

    // ---- Checking ----------------------------------------------------------------

    private async Task<(Manifest? Manifest, string? Etag)> LoadCacheAsync(CancellationToken ct)
    {
        if (!File.Exists(ManifestPath) || !File.Exists(EtagPath))
            return (null, null);

        try
        {
            var text = await File.ReadAllTextAsync(ManifestPath, ct);
            var manifest = JsonSerializer.Deserialize<Manifest>(text, OtomoHttp.Json);
            var etag = (await File.ReadAllTextAsync(EtagPath, ct)).Trim();

            if (manifest is null || manifest.Format != 1)
                return (null, null);

            return (manifest, etag);
        }
        catch (Exception e) when (e is IOException or JsonException or UnauthorizedAccessException)
        {
            _config.Log?.Invoke($"patch: ignoring unreadable cache: {e.Message}");
            return (null, null);
        }
    }

    private async Task<ManifestFetch> FetchManifestAsync(Manifest? cached, string? cachedEtag, CancellationToken ct)
    {
        var url = $"{_config.BaseUrl}/patch/v1/{Uri.EscapeDataString(_config.Channel)}/manifest";

        try
        {
            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
            timeout.CancelAfter(_config.RequestTimeout);

            using var request = new HttpRequestMessage(HttpMethod.Get, url);
            if (cached is not null && !string.IsNullOrEmpty(cachedEtag))
                request.Headers.TryAddWithoutValidation("If-None-Match", cachedEtag);

            using var response = await _http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, timeout.Token);

            // The minimum is checked on every response, before anything else.
            var headerMinimum = FirstHeader(response, "X-Min-Client-Version");
            if (IsOlder(headerMinimum))
                return new ManifestFetch { Outcome = FetchOutcome.TooOld, Minimum = headerMinimum };

            if (response.StatusCode == HttpStatusCode.NotModified)
            {
                if (cached is null)
                {
                    LastError = new OtomoError(304, "bad_response", "server returned 304 without a cached manifest", "");
                    return new ManifestFetch { Outcome = FetchOutcome.Offline };
                }

                return new ManifestFetch { Outcome = FetchOutcome.NotModified, Manifest = cached, Etag = cachedEtag };
            }

            if (!response.IsSuccessStatusCode)
            {
                LastError = await OtomoHttp.ReadErrorAsync(response);
                _config.Log?.Invoke($"patch: manifest request failed: {LastError}");
                return new ManifestFetch { Outcome = FetchOutcome.Offline };
            }

            var body = await response.Content.ReadAsStringAsync(timeout.Token);

            Manifest? parsed;
            try
            {
                parsed = JsonSerializer.Deserialize<Manifest>(body, OtomoHttp.Json);
            }
            catch (JsonException e)
            {
                LastError = new OtomoError(0, "bad_response", e.Message, "");
                _config.Log?.Invoke($"patch: manifest is not valid JSON: {e.Message}");
                return new ManifestFetch { Outcome = FetchOutcome.Offline };
            }

            if (parsed is null)
            {
                LastError = new OtomoError(0, "bad_response", "manifest body was empty", "");
                return new ManifestFetch { Outcome = FetchOutcome.Offline };
            }

            if (IsOlder(parsed.MinClientVersion))
                return new ManifestFetch { Outcome = FetchOutcome.TooOld, Minimum = parsed.MinClientVersion };

            if (parsed.Format != 1)
            {
                LastError = new OtomoError(0, "bad_response", $"unsupported manifest format {parsed.Format}", "");
                _config.Log?.Invoke($"patch: unsupported manifest format {parsed.Format}");
                return new ManifestFetch { Outcome = FetchOutcome.Offline };
            }

            return new ManifestFetch
            {
                Outcome = FetchOutcome.Ok,
                Manifest = parsed,
                Etag = response.Headers.ETag?.ToString(),
            };
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (OperationCanceledException e)
        {
            LastError = new OtomoError(0, "timeout", e.Message, "");
            _config.Log?.Invoke("patch: manifest request timed out");
            return new ManifestFetch { Outcome = FetchOutcome.Offline };
        }
        catch (HttpRequestException e)
        {
            LastError = new OtomoError(0, "network", e.Message, "", e);
            _config.Log?.Invoke($"patch: manifest request failed: {e.Message}");
            return new ManifestFetch { Outcome = FetchOutcome.Offline };
        }
        catch (Exception e) when (e is IOException or InvalidOperationException)
        {
            LastError = new OtomoError(0, "network", e.Message, "", e);
            _config.Log?.Invoke($"patch: manifest request failed: {e.Message}");
            return new ManifestFetch { Outcome = FetchOutcome.Offline };
        }
    }

    // ---- Downloading + Verifying -------------------------------------------------

    private async Task<bool> DownloadAllAsync(PatchPlan plan, CancellationToken ct)
    {
        Directory.CreateDirectory(BlobsDir);

        Interlocked.Exchange(ref _progressDone, plan.AlreadyHaveBytes);
        Interlocked.Exchange(ref _progressTotal, plan.TotalBytes);
        Interlocked.Exchange(ref _progressFilesDone, 0);
        Interlocked.Exchange(ref _progressFilesTotal, plan.FileCount);
        Progress?.Invoke(plan.AlreadyHaveBytes, plan.TotalBytes);
        MaybeRaiseProgress(force: true);

        SetState(PatchState.Downloading);

        using var gate = new SemaphoreSlim(MaxParallelDownloads);
        var tasks = new List<Task<bool>>(plan.FileCount);
        foreach (var file in plan.Files)
            tasks.Add(DownloadOneAsync(file, gate, ct));

        var results = await Task.WhenAll(tasks);
        var ok = results.All(result => result);
        if (ok)
        {
            // The final snapshot is always the exact 100% point.
            Interlocked.Exchange(ref _progressDone, plan.TotalBytes);
            Interlocked.Exchange(ref _progressFilesDone, plan.FileCount);
            MaybeRaiseProgress(force: true);
        }

        return ok;
    }

    private async Task<bool> DownloadOneAsync(PlannedFile file, SemaphoreSlim gate, CancellationToken ct)
    {
        await gate.WaitAsync(ct);
        try
        {
            _currentFile = file.Name;

            for (var attempt = 0; attempt < 2; attempt++)
            {
                SetState(PatchState.Downloading);
                if (!await DownloadToPartAsync(file.Sha256, file.Size, ct))
                    return false;

                SetState(PatchState.Verifying);
                if (await VerifyAsync(PartPath(file.Sha256), file.Sha256))
                {
                    File.Move(PartPath(file.Sha256), BlobPath(file.Sha256), overwrite: true);
                    Interlocked.Increment(ref _progressFilesDone);
                    Interlocked.Increment(ref _runFilesDownloaded);
                    MaybeRaiseProgress(force: true);
                    return true;
                }

                _config.Log?.Invoke(
                    $"patch: hash mismatch for blob {Prefix(file.Sha256)} (attempt {attempt + 1})");
                TryDelete(PartPath(file.Sha256));

                if (attempt == 1)
                {
                    LastError = new OtomoError(
                        0,
                        "hash_mismatch",
                        $"blob {file.Sha256} failed verification twice",
                        "");
                    return false;
                }
            }

            return false;
        }
        finally
        {
            gate.Release();
        }
    }

    private async Task<bool> DownloadToPartAsync(string sha256, long expectedSize, CancellationToken ct)
    {
        var part = PartPath(sha256);
        long partLength = File.Exists(part) ? new FileInfo(part).Length : 0;
        var restarted = false;

        while (true)
        {
            using var request = new HttpRequestMessage(HttpMethod.Get, BlobUrl(sha256));
            if (partLength > 0)
                request.Headers.TryAddWithoutValidation("Range", $"bytes={partLength}-");

            // Time out waiting for the headers only: the body of a large pack may take long.
            HttpResponseMessage response;
            try
            {
                using var headersTimeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
                headersTimeout.CancelAfter(_config.RequestTimeout);
                response = await _http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, headersTimeout.Token);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (OperationCanceledException e)
            {
                LastError = new OtomoError(0, "timeout", e.Message, "");
                return false;
            }
            catch (HttpRequestException e)
            {
                LastError = new OtomoError(0, "network", e.Message, "", e);
                _config.Log?.Invoke($"patch: blob {Prefix(sha256)} download failed: {e.Message}");
                return false;
            }
            catch (InvalidOperationException e)
            {
                LastError = new OtomoError(0, "network", e.Message, "", e);
                return false;
            }

            using (response)
            {
                if (response.StatusCode == HttpStatusCode.RequestedRangeNotSatisfiable)
                {
                    if (restarted)
                    {
                        LastError = new OtomoError(416, "bad_response", "range past end again", "");
                        _config.Log?.Invoke($"patch: blob {Prefix(sha256)} range restart failed");
                        return false;
                    }

                    _config.Log?.Invoke($"patch: blob {Prefix(sha256)} range past end, restarting from zero");
                    TryDelete(part);
                    partLength = 0;
                    restarted = true;
                    continue;
                }

                bool append;
                if (response.StatusCode == HttpStatusCode.PartialContent)
                {
                    append = true;
                }
                else if (response.IsSuccessStatusCode)
                {
                    append = false;
                    partLength = 0;
                }
                else
                {
                    LastError = await OtomoHttp.ReadErrorAsync(response);
                    _config.Log?.Invoke($"patch: blob {Prefix(sha256)} download failed: {LastError}");
                    return false;
                }

                // Bounds silence between reads of the body: RequestTimeout above only covers
                // getting the headers, and a body read has no timeout of its own, so a
                // connection that goes quiet mid-transfer (without actually dropping) would
                // otherwise hang here forever. CancelAfter is reset on every
                // successful read, so a slow-but-steady download is never penalised -- only a
                // gap longer than StallTimeout between two reads trips it.
                using var stall = CancellationTokenSource.CreateLinkedTokenSource(ct);
                stall.CancelAfter(_config.StallTimeout);

                try
                {
                    await using var stream = await response.Content.ReadAsStreamAsync(ct);
                    await using var output = new FileStream(
                        part,
                        append ? FileMode.Append : FileMode.Create,
                        FileAccess.Write,
                        FileShare.None,
                        BufferSize,
                        useAsync: true);

                    var buffer = new byte[BufferSize];
                    var written = partLength;
                    int read;
                    while ((read = await stream.ReadAsync(buffer.AsMemory(0, BufferSize), stall.Token)) > 0)
                    {
                        stall.CancelAfter(_config.StallTimeout);

                        // The manifest states the size: never write more than that to disk.
                        written += read;
                        if (expectedSize > 0 && written > expectedSize)
                        {
                            output.Close();
                            TryDelete(part);
                            LastError = new OtomoError(0, "size_mismatch", $"blob {sha256} is larger than {expectedSize} bytes", "");
                            _config.Log?.Invoke($"patch: blob {Prefix(sha256)} exceeds its manifest size, discarded");
                            return false;
                        }

                        await output.WriteAsync(buffer.AsMemory(0, read), ct);
                        AddProgress(read);
                    }

                    await output.FlushAsync(ct);
                    return true;
                }
                catch (OperationCanceledException) when (ct.IsCancellationRequested)
                {
                    throw;
                }
                catch (OperationCanceledException e)
                {
                    // ct is not cancelled (caught above), so this is the stall timer firing.
                    LastError = new OtomoError(0, "stalled", $"no data received for {_config.StallTimeout.TotalSeconds:0}s", "");
                    _config.Log?.Invoke($"patch: blob {Prefix(sha256)} stalled: {e.Message}");
                    return false;
                }
                catch (Exception e) when (e is IOException or HttpRequestException)
                {
                    LastError = new OtomoError(0, "network", e.Message, "", e);
                    _config.Log?.Invoke($"patch: blob {Prefix(sha256)} download failed: {e.Message}");
                    return false;
                }
            }
        }
    }

    private static Task<bool> VerifyAsync(string path, string expected) =>
        Task.Run(() =>
        {
            using var stream = File.OpenRead(path);
            using var sha = SHA256.Create();
            var hash = sha.ComputeHash(stream);
            return Convert.ToHexString(hash).ToLowerInvariant() == expected;
        });

    private void AddProgress(int bytes)
    {
        var done = Interlocked.Add(ref _progressDone, bytes);
        var downloaded = Interlocked.Add(ref _speedDownloaded, bytes);
        Interlocked.Add(ref _runBytesDownloaded, bytes);

        var now = _time.GetTimestamp();
        lock (_speedGate)
            _speedSamples.Enqueue((now, downloaded));

        Progress?.Invoke(done, _progressTotal);
        MaybeRaiseProgress(force: false);
    }

    // ---- Progress snapshots ------------------------------------------------------

    private void MaybeRaiseProgress(bool force)
    {
        var snapshot = BuildProgress();

        lock (_progressGate)
            _currentProgress = snapshot;

        if (!ShouldRaise(force))
            return;

        ProgressChanged?.Invoke(snapshot);
    }

    private PatchProgress BuildProgress()
    {
        var now = _time.GetTimestamp();
        var done = Interlocked.Read(ref _progressDone);
        var total = Interlocked.Read(ref _progressTotal);
        var filesDone = Volatile.Read(ref _progressFilesDone);
        var filesTotal = Volatile.Read(ref _progressFilesTotal);
        var speed = ComputeSpeed(now);
        var eta = speed > 0 ? (total - done) / speed : (double?)null;

        return new PatchProgress(done, total, filesDone, filesTotal, _currentFile, speed, eta);
    }

    private double ComputeSpeed(long now)
    {
        lock (_speedGate)
        {
            // Keep roughly the last SpeedWindow; always keep at least the newest sample.
            while (_speedSamples.Count > 1
                && ElapsedSeconds(now, _speedSamples.Peek().Timestamp) > SpeedWindow.TotalSeconds)
            {
                _speedSamples.Dequeue();
            }

            if (_speedSamples.Count == 0)
                return 0;

            var oldest = _speedSamples.Peek();
            var elapsed = ElapsedSeconds(now, oldest.Timestamp);
            if (elapsed <= 0)
                return 0;

            var current = Interlocked.Read(ref _speedDownloaded);
            return (current - oldest.Bytes) / elapsed;
        }
    }

    private bool ShouldRaise(bool force)
    {
        var now = _time.GetTimestamp();

        lock (_throttleGate)
        {
            if (!force
                && _lastRaiseTimestamp is long last
                && ElapsedSeconds(now, last) < ProgressInterval.TotalSeconds)
            {
                return false;
            }

            _lastRaiseTimestamp = now;
            return true;
        }
    }

    private double ElapsedSeconds(long end, long start) =>
        (double)(end - start) / _time.TimestampFrequency;

    private void ResetRun()
    {
        LastError = null;
        Current = null;
        RequiredClientVersion = null;

        Interlocked.Exchange(ref _progressDone, 0);
        Interlocked.Exchange(ref _progressTotal, 0);
        Interlocked.Exchange(ref _progressFilesDone, 0);
        Interlocked.Exchange(ref _progressFilesTotal, 0);
        Interlocked.Exchange(ref _speedDownloaded, 0);
        Interlocked.Exchange(ref _runBytesDownloaded, 0);
        Interlocked.Exchange(ref _runFilesDownloaded, 0);
        _currentFile = "";

        lock (_speedGate)
            _speedSamples.Clear();

        lock (_throttleGate)
            _lastRaiseTimestamp = null;

        lock (_progressGate)
            _currentProgress = default;
    }

    private void RaiseFinished(PatchResult result, long startTimestamp)
    {
        var duration = TimeSpan.FromSeconds(ElapsedSeconds(_time.GetTimestamp(), startTimestamp));
        var summary = new PatchSummary(
            result,
            Current?.ReleaseId,
            Interlocked.Read(ref _runBytesDownloaded),
            Volatile.Read(ref _runFilesDownloaded),
            duration,
            LastError);

        Finished?.Invoke(summary);
    }

    // ---- Mounting + saving -------------------------------------------------------

    private async Task<Dictionary<string, string>?> LoadConfigDocsAsync(Manifest manifest, CancellationToken ct)
    {
        var docs = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var pair in manifest.Config)
        {
            var path = BlobPath(pair.Value.Sha256);
            if (!File.Exists(path))
                return null;

            docs[pair.Key] = await File.ReadAllTextAsync(path, Encoding.UTF8, ct);
        }

        return docs;
    }

    private bool MountPacks(Manifest manifest)
    {
        foreach (var pack in manifest.Packs)
        {
            if (_mounted.Contains(pack.Sha256))
                continue;

            var path = BlobPath(pack.Sha256);
            if (!File.Exists(path))
            {
                LastError ??= new OtomoError(0, "missing_blob", $"pack blob {pack.Sha256} is missing", "");
                return false;
            }

            if (!_mounter.Mount(path, pack))
            {
                LastError = new OtomoError(0, "pack_mount_failed", pack.Name, "");
                _config.Log?.Invoke($"patch: failed to mount pack '{pack.Name}' ({Prefix(pack.Sha256)})");
                return false;
            }

            _mounted.Add(pack.Sha256);
        }

        return true;
    }

    private async Task<PatchResult> OfflineAsync(Manifest? cached, CancellationToken ct)
    {
        if (cached is not null
            && cached.Format == 1
            && ValidateManifest(cached)
            && AllBlobsPresent(cached))
        {
            var docs = await LoadConfigDocsAsync(cached, ct);
            if (docs is not null)
            {
                _remote.ReplaceAll(docs);
                SetState(PatchState.Mounting);
                if (!MountPacks(cached))
                    _config.Log?.Invoke("patch: offline cache mount failed; continuing offline");

                Current = cached;
                SetState(PatchState.OfflineReady);
                _config.Log?.Invoke($"patch: offline, using cached (release_id={cached.ReleaseId})");
                return PatchResult.OfflineReady;
            }
        }

        _remote.ReplaceAll(new Dictionary<string, string>());
        Current = null;
        SetState(PatchState.OfflineReady);
        _config.Log?.Invoke("patch: offline, no usable cache");
        return PatchResult.OfflineReady;
    }

    private void SaveCache(Manifest manifest, string? etag)
    {
        Directory.CreateDirectory(PatchDir);
        WriteAtomic(ManifestPath, JsonSerializer.Serialize(manifest, OtomoHttp.Json));
        WriteAtomic(EtagPath, etag ?? "");
    }

    private static void WriteAtomic(string path, string content)
    {
        var tmp = path + ".tmp";
        File.WriteAllText(tmp, content);
        File.Move(tmp, path, overwrite: true);
    }

    private void CleanupBlobs(Manifest target)
    {
        if (!Directory.Exists(BlobsDir))
            return;

        var keep = new HashSet<string>(StringComparer.Ordinal);
        foreach (var file in target.Config.Values)
            keep.Add(file.Sha256);
        foreach (var pack in target.Packs)
            keep.Add(pack.Sha256);

        foreach (var path in Directory.GetFiles(BlobsDir))
        {
            var name = Path.GetFileName(path);
            if (keep.Contains(name))
                continue;
            if (name.EndsWith(".part", StringComparison.Ordinal) && keep.Contains(name[..^5]))
                continue;

            TryDelete(path);
        }
    }

    // ---- Helpers -----------------------------------------------------------------

    private static bool ValidateManifest(Manifest manifest)
    {
        // "config": null / "packs": null mean empty.
        manifest.Config ??= new Dictionary<string, ManifestFile>();
        manifest.Packs ??= new List<ManifestPack>();

        foreach (var file in manifest.Config.Values)
            if (!IsValidSha256(file.Sha256))
                return false;

        foreach (var pack in manifest.Packs)
        {
            if (!IsValidSha256(pack.Sha256))
                return false;
            if (string.IsNullOrWhiteSpace(pack.Name))
                return false;
        }

        return true;
    }

    private static bool IsValidSha256(string? sha256)
    {
        if (sha256 is null || sha256.Length != 64)
            return false;

        foreach (var c in sha256)
        {
            var hex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
            if (!hex)
                return false;
        }

        return true;
    }

    private bool AllBlobsPresent(Manifest manifest)
    {
        foreach (var file in manifest.Config.Values)
            if (!File.Exists(BlobPath(file.Sha256)))
                return false;

        foreach (var pack in manifest.Packs)
            if (!File.Exists(BlobPath(pack.Sha256)))
                return false;

        return true;
    }

    private bool IsOlder(string? minimum) =>
        !string.IsNullOrWhiteSpace(minimum) && VersionCompare.IsOlder(_config.ClientVersion, minimum!);

    private static string? FirstHeader(HttpResponseMessage response, string name)
    {
        if (response.Headers.TryGetValues(name, out var values))
        {
            foreach (var value in values)
                return value;
        }

        return null;
    }

    private static void TryDelete(string path)
    {
        try
        {
            if (File.Exists(path))
                File.Delete(path);
        }
        catch (IOException)
        {
            // Best effort cleanup.
        }
        catch (UnauthorizedAccessException)
        {
            // Best effort cleanup.
        }
    }

    private static string Prefix(string sha256) => sha256.Length <= 8 ? sha256 : sha256[..8];

    private void SetState(PatchState state)
    {
        if (State == state)
            return;

        State = state;
        _config.Log?.Invoke($"patch: {state}");
        StateChanged?.Invoke(state);
    }

    private enum FetchOutcome
    {
        Ok,
        NotModified,
        TooOld,
        Offline,
    }

    private readonly struct ManifestFetch
    {
        public FetchOutcome Outcome { get; init; }
        public Manifest? Manifest { get; init; }
        public string? Etag { get; init; }
        public string? Minimum { get; init; }
    }
}
