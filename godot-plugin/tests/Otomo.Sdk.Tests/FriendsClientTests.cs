#nullable enable
using System;
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

public sealed class FriendsClientTests
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

    private static HttpResponseMessage Json(int status, string json) =>
        new((HttpStatusCode)status) { Content = new StringContent(json, Encoding.UTF8, "application/json") };

    private static (FriendsClient Client, FakeHandler Handler) MakeClient()
    {
        var handler = new FakeHandler();
        var client = new FriendsClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());
        return (client, handler);
    }

    [Fact]
    public async Task ListAsync_ParsesAllThreeLists()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200,
            "{\"friends\":[{\"player_id\":\"p1\",\"display_name\":\"Ann\",\"discriminator\":1,\"since\":\"2026-01-01T00:00:00Z\",\"status\":\"online\"}]," +
            "\"incoming\":[{\"player_id\":\"p2\",\"display_name\":\"Bob\",\"discriminator\":2}]," +
            "\"outgoing\":[{\"player_id\":\"p3\",\"display_name\":\"Cal\",\"discriminator\":3}]}"));

        var list = await client.ListAsync();

        var friend = Assert.Single(list.Friends);
        Assert.Equal("p1", friend.PlayerId);
        Assert.Equal("online", friend.Status);
        Assert.NotNull(friend.Since);
        Assert.Equal("Bob", Assert.Single(list.Incoming).DisplayName);
        Assert.Equal("Cal", Assert.Single(list.Outgoing).DisplayName);
        Assert.Equal(HttpMethod.Get, handler.Requests[0].Method);
        Assert.Equal("https://session.test/friends", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task RequestAsync_SendsDisplayNameAndDiscriminator_ParsesState()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200,
            "{\"player_id\":\"p1\",\"display_name\":\"Tanuki\",\"discriminator\":4417,\"state\":\"pending\"}"));

        var friend = await client.RequestAsync("Tanuki", 4417);

        Assert.Equal("pending", friend.State);
        Assert.Equal("Tanuki#4417", friend.ToString());
        Assert.Contains("\"display_name\":\"Tanuki\"", handler.Requests[0].Body);
        Assert.Contains("\"discriminator\":4417", handler.Requests[0].Body);
        Assert.Equal("https://session.test/friends/requests", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task RequestAsync_PlayerNotFound_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(404, "{\"error\":{\"code\":\"player_not_found\",\"message\":\"no such player\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.RequestAsync("Nobody", 1));

        Assert.Equal(404, error.Status);
        Assert.Equal("player_not_found", error.Code);
    }

    [Fact]
    public async Task RequestAsync_RateLimited_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ =>
        {
            var res = Json(429, "{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"too many\"}}");
            res.Headers.Add("Retry-After", "3600");
            return res;
        });

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.RequestAsync("Tanuki", 4417));

        Assert.Equal(429, error.Status);
        Assert.Equal("rate_limit_exceeded", error.Code);
    }

    [Fact]
    public async Task AcceptAsync_PostsToAcceptPathAndParsesFriend()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200,
            "{\"player_id\":\"p1\",\"display_name\":\"Ann\",\"discriminator\":1,\"state\":\"accepted\"}"));

        var friend = await client.AcceptAsync("p1");

        Assert.Equal("accepted", friend.State);
        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/friends/requests/p1/accept", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task DeclineAsync_PostsToDeclinePath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.DeclineAsync("p1");

        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/friends/requests/p1/decline", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task RemoveAsync_SendsDelete()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.RemoveAsync("p1");

        Assert.Equal(HttpMethod.Delete, handler.Requests[0].Method);
        Assert.Equal("https://session.test/friends/p1", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task BlockAsync_PostsToBlocksPath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.BlockAsync("p1");

        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/blocks/p1", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task UnblockAsync_SendsDeleteToBlocksPath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.UnblockAsync("p1");

        Assert.Equal(HttpMethod.Delete, handler.Requests[0].Method);
        Assert.Equal("https://session.test/blocks/p1", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task RequestAsync_EmptyBody_ThrowsBadResponse()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.RequestAsync("Tanuki", 4417));

        Assert.Equal("bad_response", error.Code);
    }
}
