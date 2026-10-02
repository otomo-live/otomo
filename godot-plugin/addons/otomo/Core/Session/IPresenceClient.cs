#nullable enable
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Session;

/// One player's presence, as sent to and seen from Session (guide §7.2).
public enum PresenceStatus
{
    Online,
    InMenus,
    Away,
}

/// Keeps the player marked online with Session while logged in (guide §7.2). OtomoClient
/// starts this on IAuthClient.LoggedIn and stops it on LoggedOut, so nothing here needs to be
/// wired up by the game -- only SetStatusAsync, to reflect being in a menu or away, is a game
/// call.
public interface IPresenceClient
{
    /// True while the loop from Start is running.
    bool IsRunning { get; }

    /// Starts the heartbeat loop: sends `status` immediately, then on the cadence in
    /// PresenceSchedule until StopAsync is called or `ct` is cancelled. Calling this again
    /// while already running restarts it with the new status rather than running two loops
    /// side by side (the guide's "run exactly one loop per client" rule, echoed for the
    /// events long-poll in §7.3).
    void Start(PresenceStatus status, CancellationToken ct = default);

    /// Changes the status the loop sends. Sends right away if the 10s rate limit (guide
    /// §7.2) allows it; otherwise just updates what the next scheduled tick sends, so this
    /// can never itself cause a 429. No-op if the loop isn't running.
    Task SetStatusAsync(PresenceStatus status, CancellationToken ct = default);

    /// Stops the loop. Safe to call when it isn't running, or more than once. There is
    /// deliberately no "send offline" call: the guide has Session mark a player offline on
    /// its own after 60s of missed heartbeats, so quitting needs nothing extra here.
    Task StopAsync();
}
