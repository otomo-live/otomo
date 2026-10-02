#nullable enable
using System;

namespace OtomoSdk.Http;

/// Exponential back-off with jitter (guide §4.5). Delays double up to max; each returned
/// delay adds up to +20% jitter so retries from different clients do not align.
public sealed class Backoff
{
    private static readonly TimeSpan DefaultInitial = TimeSpan.FromSeconds(1);
    private static readonly TimeSpan DefaultMax = TimeSpan.FromSeconds(30);

    private readonly TimeSpan _initial;
    private readonly TimeSpan _max;
    private readonly Random _random;
    private TimeSpan _current;

    public Backoff(TimeSpan? initial = null, TimeSpan? max = null, Random? random = null)
    {
        _initial = initial ?? DefaultInitial;
        _max = max ?? DefaultMax;
        _random = random ?? Random.Shared;
        _current = _initial;
    }

    /// Current delay plus up to +20% jitter; then advances the base (doubling, capped at max).
    public TimeSpan Next()
    {
        var baseMs = _current.TotalMilliseconds;
        var jitter = _random.NextDouble() * 0.2 * baseMs;
        var result = TimeSpan.FromMilliseconds(baseMs + jitter);

        _current = TimeSpan.FromMilliseconds(Math.Min(baseMs * 2, _max.TotalMilliseconds));
        return result;
    }

    /// Back to the initial delay.
    public void Reset() => _current = _initial;
}
