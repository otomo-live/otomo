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

public sealed class SessionClientTests
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
        new((HttpStatusCode)status)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

    [Fact]
    public async Task Init_PostsToSessionUrlWithBearer_AndParsesProfile()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(200, "{\"player_id\":\"p1\",\"display_name\":\"Bob\",\"discriminator\":7}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "token-1";
        var session = new SessionClient(http, new StubAuthClient());

        var profile = await session.InitAsync();

        Assert.Equal("p1", profile.PlayerId);
        Assert.Equal("Bob", profile.DisplayName);
        Assert.Equal(7, profile.Discriminator);
        Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/me/init", handler.Requests[0].RequestUri!.ToString());
        Assert.Equal("Bearer token-1", handler.Requests[0].Authorization);
    }

    [Fact]
    public async Task GetMe_UsesGetOnSessionUrl()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, "{\"player_id\":\"p2\"}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "token-2";
        var session = new SessionClient(http, new StubAuthClient());

        var profile = await session.GetMeAsync();

        Assert.Equal("p2", profile.PlayerId);
        Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Get, handler.Requests[0].Method);
        Assert.Equal("https://session.test/me", handler.Requests[0].RequestUri!.ToString());
        Assert.Equal("Bearer token-2", handler.Requests[0].Authorization);
    }

    [Fact]
    public async Task NotImplemented_SurfacesAsOtomoError_WithoutRefreshing()
    {
        var refreshCount = 0;
        var handler = new FakeHandler().Respond(
            _ => Json(501, "{\"error\":{\"code\":\"not_implemented\",\"message\":\"planned\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "token-1";
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            return Task.FromResult(true);
        };
        var session = new SessionClient(http, new StubAuthClient());

        var error = await Assert.ThrowsAsync<OtomoError>(() => session.InitAsync());

        Assert.Equal(501, error.Status);
        Assert.Equal("not_implemented", error.Code);
        Assert.Equal(0, refreshCount);
        Assert.Single(handler.Requests);
    }

    [Fact]
    public async Task NullBody_ThrowsBadResponse()
    {
        var handler = new FakeHandler().Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        var session = new SessionClient(http, new StubAuthClient());

        var error = await Assert.ThrowsAsync<OtomoError>(() => session.GetMeAsync());

        Assert.Equal(200, error.Status);
        Assert.Equal("bad_response", error.Code);
    }
}
