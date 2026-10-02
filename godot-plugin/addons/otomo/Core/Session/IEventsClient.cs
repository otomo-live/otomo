#nullable enable
using System;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Session;

/// One event as delivered by the long-poll (guide §7.3). Events are hints, not the truth:
/// an event says something changed; the client should still fetch the real state (e.g.
/// GET /party) to show it, using a `revision` field inside Payload (when the event carries
/// one) to ignore a stale event that arrived after a newer one was already applied.
public sealed class SessionEvent
{
    [JsonPropertyName("seq")] public long Seq { get; set; }
    [JsonPropertyName("at")] public long At { get; set; }
    [JsonPropertyName("type")] public string Type { get; set; } = "";
    [JsonPropertyName("payload")] public JsonElement Payload { get; set; }

    /// guide §7.3's Unix-millisecond `at`, converted for display/logging.
    public DateTimeOffset Time => DateTimeOffset.FromUnixTimeMilliseconds(At);

    /// Payload as raw JSON text, or "" when the event carries none (Payload is
    /// `omitempty` server-side, so an event without one deserializes to an unset element
    /// that GetRawText() would otherwise throw on).
    public string PayloadRawJson => Payload.ValueKind == JsonValueKind.Undefined ? "" : Payload.GetRawText();
}

/// Keeps the client's view of Session's event stream current while logged in (guide §7.3).
/// OtomoClient starts this on IAuthClient.LoggedIn and stops it on LoggedOut; nothing else
/// needs to be wired up by the game beyond listening to EventReceived/Resync.
public interface IEventsClient
{
    /// True while the loop from Start is running.
    bool IsRunning { get; }

    /// Starts the long-poll loop: polls from after=0, raises EventReceived for each event
    /// in order, and keeps polling until StopAsync is called or `ct` is cancelled. Calling
    /// this again while already running restarts it from after=0 rather than running two
    /// loops side by side -- the guide's "run exactly one loop per client" rule (§7.3),
    /// which the server enforces anyway by cancelling the first poll when a second starts.
    void Start(CancellationToken ct = default);

    /// Stops the loop. Safe to call when it isn't running, or more than once.
    Task StopAsync();

    /// Raised once per event, in the order the server returned them. The cursor (`seq`) is
    /// tracked internally; callers never need to pass one back in.
    event Action<SessionEvent>? EventReceived;

    /// Raised when the server answers `{"resync":true}` (guide §7.3: the cursor is too old,
    /// or the stream expired and restarted). EventsClient resets its own cursor to 0 and
    /// keeps polling either way; the guide's fix on the caller's side is to re-fetch friends
    /// and party with normal calls, since some events in between were never delivered.
    event Action? Resync;
}
