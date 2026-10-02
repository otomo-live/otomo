#nullable enable
using System;
using OtomoSdk.Http;
using OtomoSdk.Session;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class PresenceScheduleTests
{
    [Fact]
    public void NextDelay_OnSuccess_ReturnsTheSteadyInterval()
    {
        var schedule = new PresenceSchedule(TimeSpan.FromSeconds(20), new Backoff(random: new Random(1)));

        Assert.Equal(TimeSpan.FromSeconds(20), schedule.NextDelay(sendSucceeded: true));
        Assert.Equal(TimeSpan.FromSeconds(20), schedule.NextDelay(sendSucceeded: true));
    }

    [Fact]
    public void NextDelay_OnFailure_BacksOffLikeBackoffNext()
    {
        var schedule = new PresenceSchedule(TimeSpan.FromSeconds(20), new Backoff(random: new Random(1234)));
        var bases = new[] { 1.0, 2.0, 4.0 };

        foreach (var seconds in bases)
        {
            var value = schedule.NextDelay(sendSucceeded: false).TotalSeconds;
            Assert.True(value >= seconds, $"expected >= {seconds}s, got {value}s");
            Assert.True(value <= seconds * 1.2, $"expected <= {seconds * 1.2}s, got {value}s");
        }
    }

    [Fact]
    public void NextDelay_SuccessAfterFailures_ResetsBackoffForTheNextFailure()
    {
        var schedule = new PresenceSchedule(TimeSpan.FromSeconds(20), new Backoff(random: new Random(7)));

        schedule.NextDelay(sendSucceeded: false);
        schedule.NextDelay(sendSucceeded: false);
        schedule.NextDelay(sendSucceeded: true);

        var value = schedule.NextDelay(sendSucceeded: false).TotalSeconds;
        Assert.True(value >= 1.0, $"expected >= 1s, got {value}s");
        Assert.True(value <= 1.2, $"expected <= 1.2s, got {value}s");
    }

    [Fact]
    public void DefaultInterval_Is20Seconds()
    {
        Assert.Equal(TimeSpan.FromSeconds(20), PresenceSchedule.DefaultInterval);
    }

    [Fact]
    public void NoArgsConstructor_UsesDefaultIntervalAndOwnBackoff()
    {
        var schedule = new PresenceSchedule();
        Assert.Equal(PresenceSchedule.DefaultInterval, schedule.Interval);
        Assert.Equal(TimeSpan.FromSeconds(20), schedule.NextDelay(sendSucceeded: true));
    }
}
