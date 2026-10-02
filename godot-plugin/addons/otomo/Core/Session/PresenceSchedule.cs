#nullable enable
using System;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// Pure scheduling policy for PresenceClient's loop: how long to wait before the next send,
/// given whether the last one succeeded. Kept separate from the actual async loop so cadence
/// and back-off are unit-testable without any real waiting or a fake timer.
public sealed class PresenceSchedule
{
    public static readonly TimeSpan DefaultInterval = TimeSpan.FromSeconds(20);

    public TimeSpan Interval { get; }
    private readonly Backoff _backoff;

    public PresenceSchedule(TimeSpan? interval = null, Backoff? backoff = null)
    {
        Interval = interval ?? DefaultInterval;
        _backoff = backoff ?? new Backoff();
    }

    /// Call once after every send attempt. Returns how long to wait before the next one:
    /// the steady cadence on success, or the next back-off step on failure (reset once a
    /// send succeeds again).
    public TimeSpan NextDelay(bool sendSucceeded)
    {
        if (sendSucceeded)
        {
            _backoff.Reset();
            return Interval;
        }

        return _backoff.Next();
    }
}
