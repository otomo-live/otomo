#nullable enable
using System;
using System.Net.Http;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// <inheritdoc cref="IEventsClient"/>
public sealed class EventsClient : IEventsClient
{
    /// guide §7.3: the server holds the request up to 25s; use a longer client-side timeout
    /// for this call only so a slow-but-alive long-poll is never mistaken for a dead one.
    public static readonly TimeSpan PollTimeout = TimeSpan.FromSeconds(35);

    private readonly OtomoHttp _http;
    private readonly IAuthClient _auth;
    private readonly object _gate = new();

    private CancellationTokenSource? _loopCts;
    private Task? _loopTask;

    public EventsClient(OtomoHttp http, IAuthClient auth)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _auth = auth ?? throw new ArgumentNullException(nameof(auth));
    }

    public event Action<SessionEvent>? EventReceived;
    public event Action? Resync;

    public bool IsRunning
    {
        get { lock (_gate) return _loopTask is not null; }
    }

    public void Start(CancellationToken ct = default)
    {
        CancellationTokenSource? old;

        lock (_gate)
        {
            old = _loopCts;

            var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            _loopCts = cts;
            _loopTask = Task.Run(() => RunLoopAsync(cts.Token));
        }

        // Torn down after starting the replacement, so IsRunning is never briefly false
        // when Start is called again while already running.
        old?.Cancel();
        old?.Dispose();
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

    private async Task RunLoopAsync(CancellationToken ct)
    {
        var backoff = new Backoff();
        long after = 0;

        try
        {
            while (true)
            {
                JsonElement result;
                try
                {
                    result = await _http.SendAsync<JsonElement>(
                        HttpMethod.Get,
                        _auth.SessionBaseUrl + $"/events?after={after}",
                        timeout: PollTimeout,
                        ct: ct);
                    backoff.Reset();
                }
                catch (OtomoError)
                {
                    // Network hiccup, 5xx, or (after the shared layer's own refresh+retry)
                    // an unrecoverable 401 -- guide §4.5's back-off applies to all of these
                    // for a background loop like this one; a button press would instead
                    // surface the error, but nothing here is a button press.
                    await Task.Delay(backoff.Next(), ct);
                    continue;
                }

                if (result.ValueKind == JsonValueKind.Object &&
                    result.TryGetProperty("resync", out var resyncFlag) &&
                    resyncFlag.ValueKind == JsonValueKind.True)
                {
                    after = 0;
                    Resync?.Invoke();
                    continue;
                }

                if (result.ValueKind == JsonValueKind.Array)
                {
                    foreach (var element in result.EnumerateArray())
                    {
                        var evt = element.Deserialize<SessionEvent>(OtomoHttp.Json);
                        if (evt is null)
                            continue;

                        if (evt.Seq > after)
                            after = evt.Seq;

                        EventReceived?.Invoke(evt);
                    }
                }

                // An empty array (the hold timed out with nothing to report) and a handled
                // resync both mean "poll again immediately" (guide §7.3) -- the server's own
                // hold is the only pacing this loop needs on the happy path.
            }
        }
        catch (OperationCanceledException)
        {
            // Normal shutdown via StopAsync or the ct passed to Start.
        }
    }
}
