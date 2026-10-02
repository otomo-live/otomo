#nullable enable
using System;
using System.Collections.Generic;
using System.Net;
using System.Net.Http;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk;
using OtomoSdk.Auth;
using OtomoSdk.Http;
using OtomoSdk.Session;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class PresenceClientTests
{
    private sealed class StubAuthClient : IAuthClient
    {
        public bool IsLoggedIn => AccessToken is not null;
        public string? AccessToken { get; set; } = "token-1";
        public string SessionBaseUrl { get; set; } = "https://session.test";
        public Task LoginWithDeviceIdAsync(CancellationToken ct = default) => Task.CompletedTask;
        public Task<bool> RefreshAccessTokenAsync(CancellationToken ct = default) => Task.FromResult(false);
        public Task StartAsync(CancellationToken ct = default) => Task.CompletedTask;
        public Task LogoutAsync(CancellationToken ct = default) => Task.CompletedTask;
        public event Action? LoggedIn { add { } remove { } }
        public event Action? LoggedOut { add { } remove { } }
    }

    /// Manual clock so the 10s rate-limit gate in SetStatusAsync is fully deterministic.
    private sealed class FakeTimeProvider : TimeProvider
    {
        private long _timestamp;

        public override long TimestampFrequency => TimeSpan.TicksPerSecond;

        public override long GetTimestamp() => _timestamp;

        public void Advance(TimeSpan delta) => _timestamp += delta.Ticks;
    }

    private static OtomoConfig Config() =>
        new("https://example.test", "/tmp/otomo") { RequestTimeout = TimeSpan.FromSeconds(5) };

    private static HttpResponseMessage NoContent() => new(HttpStatusCode.NoContent);

    private static HttpResponseMessage ServerError(string code) =>
        new(HttpStatusCode.InternalServerError)
        {
            Content = new StringContent(
                $"{{\"error\":{{\"code\":\"{code}\",\"message\":\"kaput\"}}}}", Encoding.UTF8, "application/json"),
        };

    /// The FallbackResponder callback only gets the raw HttpRequestMessage (FakeHandler
    /// records the string body separately, after the responder runs), so tests that need
    /// to branch on the body inside the responder itself read it synchronously here.
    private static string ReadBody(HttpRequestMessage request) =>
        request.Content?.ReadAsStringAsync().GetAwaiter().GetResult() ?? "";

    private static async Task AwaitOrTimeout(Task task) =>
        await task.WaitAsync(TimeSpan.FromSeconds(5));

    [Fact]
    public async Task Start_SendsHeartbeatImmediately()
    {
        var sent = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            sent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(sent.Task);
        await client.StopAsync();

        var request = handler.Requests[0];
        Assert.Equal(HttpMethod.Post, request.Method);
        Assert.Equal("https://session.test/presence/heartbeat", request.RequestUri!.ToString());
        Assert.Contains("\"status\":\"online\"", request.Body);
    }

    [Theory]
    [InlineData(PresenceStatus.Online, "online")]
    [InlineData(PresenceStatus.InMenus, "in_menus")]
    [InlineData(PresenceStatus.Away, "away")]
    public async Task Start_SendsTheStatusNameTheGuideExpects(PresenceStatus status, string expected)
    {
        var sent = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            sent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        client.Start(status);
        await AwaitOrTimeout(sent.Task);
        await client.StopAsync();

        Assert.Contains($"\"status\":\"{expected}\"", handler.Requests[0].Body);
    }

    [Fact]
    public async Task IsRunning_ReflectsLifecycle()
    {
        var sent = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            sent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        Assert.False(client.IsRunning);

        client.Start(PresenceStatus.Online);
        Assert.True(client.IsRunning);

        await AwaitOrTimeout(sent.Task);
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }

    [Fact]
    public async Task StopAsync_WhenNeverStarted_IsSafeAndIdempotent()
    {
        var http = new OtomoHttp(new HttpClient(new FakeHandler()), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        await client.StopAsync();
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }

    [Fact]
    public async Task StopAsync_CalledTwiceAfterRunning_IsSafe()
    {
        var sent = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            sent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(sent.Task);

        await client.StopAsync();
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }

    [Fact]
    public async Task Start_CalledAgainWhileRunning_RestartsWithTheNewStatus()
    {
        var firstSent = new TaskCompletionSource();
        var secondSent = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = request =>
        {
            var body = ReadBody(request);
            if (body.Contains("online"))
                firstSent.TrySetResult();
            else if (body.Contains("away"))
                secondSent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(firstSent.Task);

        client.Start(PresenceStatus.Away);
        await AwaitOrTimeout(secondSent.Task);

        await client.StopAsync();

        // Exactly one immediate send per Start call -- restarting replaced the old loop
        // rather than running a second one alongside it.
        Assert.Equal(2, handler.Requests.Count);
    }

    [Fact]
    public async Task SetStatusAsync_AfterMinIntervalElapsed_SendsImmediately()
    {
        var clock = new FakeTimeProvider();
        var firstSent = new TaskCompletionSource();
        var bodies = new List<string>();
        var handler = new FakeHandler();
        handler.FallbackResponder = request =>
        {
            var body = ReadBody(request);
            lock (bodies) bodies.Add(body);
            firstSent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient(), clock);

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(firstSent.Task);

        clock.Advance(PresenceClient.MinInterval);
        await client.SetStatusAsync(PresenceStatus.Away);
        await client.StopAsync();

        Assert.Equal(2, bodies.Count);
        Assert.Contains("\"status\":\"online\"", bodies[0]);
        Assert.Contains("\"status\":\"away\"", bodies[1]);
    }

    [Fact]
    public async Task SetStatusAsync_BeforeMinIntervalElapsed_DoesNotSendImmediately()
    {
        var clock = new FakeTimeProvider();
        var firstSent = new TaskCompletionSource();
        var bodies = new List<string>();
        var handler = new FakeHandler();
        handler.FallbackResponder = request =>
        {
            var body = ReadBody(request);
            lock (bodies) bodies.Add(body);
            firstSent.TrySetResult();
            return NoContent();
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient(), clock);

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(firstSent.Task);

        // No time advanced: still inside the 10s rate limit, so this must not send yet.
        await client.SetStatusAsync(PresenceStatus.Away);
        await client.StopAsync();

        Assert.Single(bodies);
    }

    [Fact]
    public async Task SetStatusAsync_WhenNotRunning_IsNoOp()
    {
        var handler = new FakeHandler();
        handler.FallbackResponder = _ => throw new InvalidOperationException("should not send");
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        await client.SetStatusAsync(PresenceStatus.Away);

        Assert.Empty(handler.Requests);
    }

    [Fact]
    public async Task FailedHeartbeat_IsSwallowedAndTheLoopKeepsRunning()
    {
        var attempted = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            attempted.TrySetResult();
            return ServerError("boom");
        };
        var http = new OtomoHttp(new HttpClient(handler), Config());
        var client = new PresenceClient(http, new StubAuthClient());

        client.Start(PresenceStatus.Online);
        await AwaitOrTimeout(attempted.Task);

        Assert.True(client.IsRunning);
        await client.StopAsync();
        Assert.False(client.IsRunning);
    }
}
