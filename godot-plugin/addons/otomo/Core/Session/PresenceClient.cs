#nullable enable
using System;
using System.Net.Http;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// <inheritdoc cref="IPresenceClient"/>
public sealed class PresenceClient : IPresenceClient
{
    /// guide §7.2: the server refuses a heartbeat sooner than this with 429. The loop's own
    /// cadence (PresenceSchedule.DefaultInterval, 20s) is already well above this; only an
    /// explicit SetStatusAsync could otherwise come in under it.
    public static readonly TimeSpan MinInterval = TimeSpan.FromSeconds(10);

    private readonly OtomoHttp _http;
    private readonly IAuthClient _auth;
    private readonly TimeProvider _time;
    private readonly object _gate = new();

    private CancellationTokenSource? _loopCts;
    private Task? _loopTask;
    private PresenceStatus _status;
    private long? _lastSendTimestamp;

    public PresenceClient(OtomoHttp http, IAuthClient auth, TimeProvider? timeProvider = null)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _auth = auth ?? throw new ArgumentNullException(nameof(auth));
        _time = timeProvider ?? TimeProvider.System;
    }

    public bool IsRunning
    {
        get { lock (_gate) return _loopTask is not null; }
    }

    public void Start(PresenceStatus status, CancellationToken ct = default)
    {
        CancellationTokenSource? old;

        lock (_gate)
        {
            old = _loopCts;

            _status = status;
            var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            _loopCts = cts;
            _loopTask = Task.Run(() => RunLoopAsync(cts.Token));
        }

        // Torn down after starting the replacement, so IsRunning is never briefly false when
        // Start is called again while already running.
        old?.Cancel();
        old?.Dispose();
    }

    public async Task SetStatusAsync(PresenceStatus status, CancellationToken ct = default)
    {
        bool dueNow;

        lock (_gate)
        {
            if (_loopTask is null)
                return;

            _status = status;
            dueNow = IsDueLocked();
        }

        if (dueNow)
            await TrySendAsync(status, ct);
    }

    public async Task StopAsync()
    {
        Task? loop;
        CancellationTokenSource? cts;

        lock (_gate)
        {
            loop = _loopTask;
            cts = _loopCts;
            _loopTask = null;
            _loopCts = null;
        }

        cts?.Cancel();
        if (loop is not null)
        {
            try
            {
                await loop;
            }
            catch (OperationCanceledException)
            {
            }
        }

        cts?.Dispose();
    }

    private bool IsDueLocked() =>
        _lastSendTimestamp is not long last || _time.GetElapsedTime(last) >= MinInterval;

    private async Task RunLoopAsync(CancellationToken ct)
    {
        var schedule = new PresenceSchedule();
        try
        {
            while (true)
            {
                PresenceStatus status;
                lock (_gate) status = _status;

                var ok = await TrySendAsync(status, ct);
                await Task.Delay(schedule.NextDelay(ok), ct);
            }
        }
        catch (OperationCanceledException)
        {
            // Normal shutdown via StopAsync or the ct passed to Start.
        }
    }

    private async Task<bool> TrySendAsync(PresenceStatus status, CancellationToken ct)
    {
        try
        {
            var body = new HeartbeatBody { Status = StatusName(status) };
            await _http.SendAsync(HttpMethod.Post, _auth.SessionBaseUrl + "/presence/heartbeat", body, ct: ct);
            lock (_gate) _lastSendTimestamp = _time.GetTimestamp();
            return true;
        }
        catch (OtomoError)
        {
            // A missed heartbeat is harmless on its own (guide §7.2: the server only marks
            // the player offline after 60s of silence) and there is no UI for "a heartbeat
            // failed", so this is swallowed here; the loop backs off and retries.
            return false;
        }
    }

    private static string StatusName(PresenceStatus status) => status switch
    {
        PresenceStatus.Online => "online",
        PresenceStatus.InMenus => "in_menus",
        PresenceStatus.Away => "away",
        _ => throw new ArgumentOutOfRangeException(nameof(status), status, null),
    };

    private sealed class HeartbeatBody
    {
        [JsonPropertyName("status")] public string Status { get; set; } = "";
    }
}
