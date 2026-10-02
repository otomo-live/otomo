#nullable enable
using System;
using OtomoSdk.Http;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class BackoffTests
{
    [Fact]
    public void Next_DoublesAndCapsAtDefaultMax()
    {
        var backoff = new Backoff(random: new Random(1234));
        var bases = new[] { 1.0, 2.0, 4.0, 8.0, 16.0, 30.0, 30.0, 30.0 };

        foreach (var seconds in bases)
        {
            var value = backoff.Next().TotalSeconds;
            Assert.True(value >= seconds, $"expected >= {seconds}s, got {value}s");
            Assert.True(value <= seconds * 1.2, $"expected <= {seconds * 1.2}s, got {value}s");
        }
    }

    [Fact]
    public void Reset_ReturnsToInitialDelay()
    {
        var backoff = new Backoff(random: new Random(7));
        backoff.Next();
        backoff.Next();
        backoff.Next();
        backoff.Reset();

        var value = backoff.Next().TotalSeconds;
        Assert.True(value >= 1.0, $"expected >= 1s, got {value}s");
        Assert.True(value <= 1.2, $"expected <= 1.2s, got {value}s");
    }

    [Fact]
    public void CustomInitialAndMax_AreRespected()
    {
        var backoff = new Backoff(
            TimeSpan.FromMilliseconds(100),
            TimeSpan.FromMilliseconds(250),
            new Random(3));
        var bases = new[] { 100.0, 200.0, 250.0, 250.0 };

        foreach (var ms in bases)
        {
            var value = backoff.Next().TotalMilliseconds;
            Assert.True(value >= ms, $"expected >= {ms}ms, got {value}ms");
            Assert.True(value <= ms * 1.2, $"expected <= {ms * 1.2}ms, got {value}ms");
        }
    }
}
