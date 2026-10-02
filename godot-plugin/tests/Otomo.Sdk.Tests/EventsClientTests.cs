#nullable enable
using System;
using System.Collections.Generic;
using System.Linq;
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

public sealed class EventsClientTests
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

    private static OtomoConfig Config() =>
        new("https://example.test", "/tmp/otomo") { RequestTimeout = TimeSpan.FromSeconds(5) };

    private static HttpResponseMessage Json(string json) =>
        new(HttpStatusCode.OK) { Content = new StringContent(json, Encoding.UTF8, "application/json") };

    private static HttpResponseMessage EmptyArray() => Json("[]");

    private static HttpResponseMessage ResyncResponse() => Json("{\"resync\":true}");

    private static HttpResponseMessage EventsResponse(params (long Seq, string Type, string? PayloadJson)[] events)
    {
        var items = events.Select(e =>
            e.PayloadJson is null
                ? $"{{\"seq\":{e.Seq},\"at\":1000,\"type\":\"{e.Type}\"}}"
                : $"{{\"seq\":{e.Seq},\"at\":1000,\"type\":\"{e.Type}\",\"payload\":{e.PayloadJson}}}");
        return Json("[" + string.Join(",", items) + "]");
    }

    private static Task AwaitOrTimeout(Task task) => task.WaitAsync(TimeSpan.FromSeconds(5));

    private static Task<T> AwaitOrTimeout<T>(Task<T> task) => task.WaitAsync(TimeSpan.FromSeconds(5));

    [Fact]
    public async Task Start_PollsWithAfterZeroOnFirstRequest()
    {
        var firstRequest = new TaskCompletionSource<Uri>();
        var handler = new FakeHandler();
        handler.FallbackResponder = request =>
        {
            firstRequest.TrySetResult(request.RequestUri!);
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        client.Start();
        var uri = await AwaitOrTimeout(firstRequest.Task);
        await client.StopAsync();

        Assert.Equal("https://session.test/events?after=0", uri.ToString());
    }

    [Fact]
    public async Task EventsReceived_AreRaisedInOrder_AndCursorAdvancesToHighestSeq()
    {
        var secondRequest = new TaskCompletionSource<Uri>();
        var handler = new FakeHandler().Respond(_ => EventsResponse(
            (1, "friend.request", "{\"player_id\":\"p1\"}"),
            (2, "party.updated", "{\"party_id\":\"pt1\",\"revision\":5}")));
        handler.FallbackResponder = request =>
        {
            secondRequest.TrySetResult(request.RequestUri!);
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        var received = new List<SessionEvent>();
        client.EventReceived += received.Add;

        client.Start();
        var uri = await AwaitOrTimeout(secondRequest.Task);
        await client.StopAsync();

        Assert.Equal(2, received.Count);
        Assert.Equal(1, received[0].Seq);
        Assert.Equal("friend.request", received[0].Type);
        Assert.Contains("\"player_id\":\"p1\"", received[0].PayloadRawJson);
        Assert.Equal(2, received[1].Seq);
        Assert.Equal("party.updated", received[1].Type);
        Assert.Equal("https://session.test/events?after=2", uri.ToString());
    }

    [Fact]
    public async Task UnknownEventType_IsStillDeliveredRatherThanDropped()
    {
        var secondRequest = new TaskCompletionSource();
        var handler = new FakeHandler().Respond(_ => EventsResponse((1, "some_future_event", null)));
        handler.FallbackResponder = _ =>
        {
            secondRequest.TrySetResult();
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        var received = new List<SessionEvent>();
        client.EventReceived += received.Add;

        client.Start();
        await AwaitOrTimeout(secondRequest.Task);
        await client.StopAsync();

        var evt = Assert.Single(received);
        Assert.Equal("some_future_event", evt.Type);
        Assert.Equal("", evt.PayloadRawJson);
    }

    [Fact]
    public async Task Resync_ResetsCursorToZeroAndRaisesResync()
    {
        var handler = new FakeHandler()
            .Respond(_ => EventsResponse((5, "party.updated", "{}")))
            .Respond(_ => ResyncResponse());
        var pollAfterResync = new TaskCompletionSource<Uri>();
        handler.FallbackResponder = request =>
        {
            pollAfterResync.TrySetResult(request.RequestUri!);
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        var resyncCount = 0;
        client.Resync += () => resyncCount++;

        client.Start();
        var uri = await AwaitOrTimeout(pollAfterResync.Task);
        await client.StopAsync();

        Assert.Equal(1, resyncCount);
        Assert.Equal("https://session.test/events?after=0", uri.ToString());
    }

    [Fact]
    public async Task EmptyArray_PollsAgainImmediatelyWithoutDelay()
    {
        var secondRequest = new TaskCompletionSource();
        var count = 0;
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            if (Interlocked.Increment(ref count) == 2)
                secondRequest.TrySetResult();
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        client.Start();
        await AwaitOrTimeout(secondRequest.Task);
        await client.StopAsync();

        Assert.True(count >= 2, $"expected at least 2 polls, got {count}");
    }

    [Fact]
    public async Task NetworkError_BacksOffBeforeRetrying()
    {
        var attempts = 0;
        var firstAttemptAt = DateTime.UtcNow;
        var secondAttempt = new TaskCompletionSource<DateTime>();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            var n = Interlocked.Increment(ref attempts);
            if (n == 1)
            {
                firstAttemptAt = DateTime.UtcNow;
                throw new HttpRequestException("down");
            }

            secondAttempt.TrySetResult(DateTime.UtcNow);
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        client.Start();
        var secondAt = await AwaitOrTimeout(secondAttempt.Task);
        await client.StopAsync();

        Assert.True(
            secondAt - firstAttemptAt >= TimeSpan.FromMilliseconds(900),
            $"expected a ~1s back-off before the retry, got {(secondAt - firstAttemptAt).TotalMilliseconds}ms");
    }

    [Fact]
    public async Task IsRunning_ReflectsLifecycle()
    {
        var firstRequest = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            firstRequest.TrySetResult();
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        Assert.False(client.IsRunning);

        client.Start();
        Assert.True(client.IsRunning);

        await AwaitOrTimeout(firstRequest.Task);
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }

    [Fact]
    public async Task StopAsync_WhenNeverStarted_IsSafeAndIdempotent()
    {
        var client = new EventsClient(new OtomoHttp(new HttpClient(new FakeHandler()), Config()), new StubAuthClient());

        await client.StopAsync();
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }

    [Fact]
    public async Task StopAsync_CalledTwiceAfterRunning_IsSafe()
    {
        var firstRequest = new TaskCompletionSource();
        var handler = new FakeHandler();
        handler.FallbackResponder = _ =>
        {
            firstRequest.TrySetResult();
            return EmptyArray();
        };
        var client = new EventsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());

        client.Start();
        await AwaitOrTimeout(firstRequest.Task);

        await client.StopAsync();
        await client.StopAsync();

        Assert.False(client.IsRunning);
    }
}
