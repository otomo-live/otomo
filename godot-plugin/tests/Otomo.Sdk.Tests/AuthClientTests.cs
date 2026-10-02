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

public sealed class AuthClientTests
{
    private static readonly List<string> AllLogs = new();

    private sealed class MemorySecretStore : ISecretStore
    {
        public string DeviceId { get; set; } = "device-default";
        public string? RefreshToken { get; set; }
        public int ClearCount { get; private set; }

        public string LoadOrCreateDeviceId() => DeviceId;

        public string? LoadRefreshToken() => RefreshToken;

        public void SaveRefreshToken(string token) => RefreshToken = token;

        public void ClearRefreshToken()
        {
            RefreshToken = null;
            ClearCount++;
        }
    }

    private static OtomoConfig NewConfig() =>
        new("https://example.test", "/tmp/otomo")
        {
            RequestTimeout = TimeSpan.FromSeconds(5),
            Log = line =>
            {
                lock (AllLogs)
                    AllLogs.Add(line);
            },
        };

    private static HttpResponseMessage Json(int status, string json) =>
        new((HttpStatusCode)status)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

    private static string LoginJson(string access, string refresh, string? session) =>
        "{\"schema_version\":1,\"access_token\":\"" + access + "\",\"expires_in\":900," +
        "\"refresh_token\":\"" + refresh + "\"" +
        (session is null ? "" : ",\"services\":{\"session\":\"" + session + "\"}") + "}";

    private static void AssertNoSecretLeaks(params string[] secrets)
    {
        string[] lines;
        lock (AllLogs)
            lines = AllLogs.ToArray();

        foreach (var line in lines)
            foreach (var secret in secrets)
                Assert.DoesNotContain(secret, line, StringComparison.Ordinal);
    }

    [Fact]
    public async Task Login_SavesRefreshTokenAndSetsState()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(200, LoginJson("access-login", "refresh-login", "https://sess.example.com/")));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-login" };
        var auth = new AuthClient(http, store, config);
        var loggedIn = 0;
        auth.LoggedIn += () => loggedIn++;

        await auth.LoginWithDeviceIdAsync();

        Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.EndsWith("/auth/anonymous", handler.Requests[0].RequestUri!.AbsolutePath);
        Assert.Contains("\"device_id\":\"device-login\"", handler.Requests[0].Body!);
        Assert.Equal("refresh-login", store.RefreshToken);
        Assert.Equal("access-login", auth.AccessToken);
        Assert.True(auth.IsLoggedIn);
        Assert.Equal(1, loggedIn);
        Assert.Equal("https://sess.example.com", auth.SessionBaseUrl);
        AssertNoSecretLeaks("device-login", "access-login", "refresh-login");
    }

    [Theory]
    [InlineData("{\"access_token\":\"access-fallback\",\"refresh_token\":\"refresh-fallback\"}")]
    [InlineData("{\"access_token\":\"access-fallback\",\"refresh_token\":\"refresh-fallback\",\"services\":null}")]
    [InlineData("{\"access_token\":\"access-fallback\",\"refresh_token\":\"refresh-fallback\",\"services\":{\"session\":null}}")]
    [InlineData("{\"access_token\":\"access-fallback\",\"refresh_token\":\"refresh-fallback\",\"services\":{\"session\":\"session\"}}")]
    public async Task Login_UsesFallbackSessionUrl(string loginJson)
    {
        var handler = new FakeHandler().Respond(_ => Json(200, loginJson));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-fallback" };
        var auth = new AuthClient(http, store, config);

        await auth.LoginWithDeviceIdAsync();

        Assert.Equal("https://example.test/api/player/session", auth.SessionBaseUrl);
        AssertNoSecretLeaks("device-fallback", "access-fallback", "refresh-fallback");
    }

    [Fact]
    public async Task Refresh_RotatesSavedToken()
    {
        var handler = new FakeHandler()
            .Respond(_ => Json(200, LoginJson("access-old", "refresh-old", "https://sess.example.com")))
            .Respond(_ => Json(200, LoginJson("access-new", "refresh-new", "https://sess.example.com")));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-refresh" };
        var auth = new AuthClient(http, store, config);

        await auth.LoginWithDeviceIdAsync();
        var refreshed = await auth.RefreshAccessTokenAsync();

        Assert.True(refreshed);
        Assert.Equal(2, handler.Requests.Count);
        Assert.EndsWith("/auth/refresh", handler.Requests[1].RequestUri!.AbsolutePath);
        Assert.Contains("\"refresh_token\":\"refresh-old\"", handler.Requests[1].Body!);
        Assert.Equal("refresh-new", store.RefreshToken);
        Assert.Equal("access-new", auth.AccessToken);
        AssertNoSecretLeaks("device-refresh", "access-old", "access-new", "refresh-old", "refresh-new");
    }

    [Fact]
    public async Task Concurrent401_RefreshesExactlyOnce()
    {
        var handler = new FakeHandler { Delay = TimeSpan.FromMilliseconds(30) };
        handler.FallbackResponder = request =>
        {
            var path = request.RequestUri!.AbsolutePath;
            var bearer = request.Headers.Authorization?.Parameter;

            if (path.EndsWith("/auth/anonymous", StringComparison.Ordinal))
                return Json(200, LoginJson("access-old", "refresh-old", "https://sess.example.com"));
            if (path.EndsWith("/auth/refresh", StringComparison.Ordinal))
                return Json(200, LoginJson("access-new", "refresh-new", "https://sess.example.com"));
            if (bearer == "access-old")
                return Json(401, "{\"error\":{\"code\":\"expired\",\"message\":\"no\"}}");

            return Json(200, "{\"player_id\":\"p1\",\"display_name\":\"Bob\"}");
        };
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-concurrent" };
        var auth = new AuthClient(http, store, config);

        await auth.LoginWithDeviceIdAsync();

        var first = http.SendAsync<PlayerProfile>(HttpMethod.Get, "/api/player/me");
        var second = http.SendAsync<PlayerProfile>(HttpMethod.Get, "/api/player/me");
        var results = await Task.WhenAll(first, second);

        Assert.NotNull(results[0]);
        Assert.NotNull(results[1]);
        Assert.Equal("access-new", auth.AccessToken);
        Assert.Equal("refresh-new", store.RefreshToken);

        var refreshCount = handler.Requests.Count(
            r => r.RequestUri!.AbsolutePath.EndsWith("/auth/refresh", StringComparison.Ordinal));
        Assert.Equal(1, refreshCount);
        AssertNoSecretLeaks("device-concurrent", "access-old", "access-new", "refresh-old", "refresh-new");
    }

    [Fact]
    public async Task Refresh401_LogsInWithDeviceId()
    {
        var handler = new FakeHandler()
            .Respond(_ => Json(401, "{\"error\":{\"code\":\"invalid_token\",\"message\":\"no\"}}"))
            .Respond(_ => Json(200, LoginJson("access-relogin", "refresh-relogin", "https://sess.example.com")));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-relogin", RefreshToken = "refresh-stale" };
        var auth = new AuthClient(http, store, config);

        var refreshed = await auth.RefreshAccessTokenAsync();

        Assert.True(refreshed);
        Assert.Equal("access-relogin", auth.AccessToken);
        Assert.Equal("refresh-relogin", store.RefreshToken);
        Assert.Equal(2, handler.Requests.Count);
        Assert.EndsWith("/auth/refresh", handler.Requests[0].RequestUri!.AbsolutePath);
        Assert.EndsWith("/auth/anonymous", handler.Requests[1].RequestUri!.AbsolutePath);
        AssertNoSecretLeaks("device-relogin", "refresh-stale", "access-relogin", "refresh-relogin");
    }

    [Fact]
    public async Task Refresh401_AndDeviceLogin500_ReturnsFalseAndRaisesLoggedOut()
    {
        var handler = new FakeHandler()
            .Respond(_ => Json(401, "{\"error\":{\"code\":\"invalid_token\",\"message\":\"no\"}}"))
            .Respond(_ => Json(500, "{\"error\":{\"code\":\"boom\",\"message\":\"kaput\"}}"));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-boom", RefreshToken = "refresh-stale" };
        var auth = new AuthClient(http, store, config);
        var loggedOut = 0;
        auth.LoggedOut += () => loggedOut++;

        var refreshed = await auth.RefreshAccessTokenAsync();

        Assert.False(refreshed);
        Assert.Null(auth.AccessToken);
        Assert.Equal(1, loggedOut);
        AssertNoSecretLeaks("device-boom", "refresh-stale");
    }

    [Fact]
    public async Task RefreshNetworkError_KeepsSavedToken()
    {
        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("dns failed"));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-net", RefreshToken = "refresh-stale" };
        var auth = new AuthClient(http, store, config);

        var refreshed = await auth.RefreshAccessTokenAsync();

        Assert.False(refreshed);
        Assert.Equal("refresh-stale", store.RefreshToken);
        Assert.Null(auth.AccessToken);
        AssertNoSecretLeaks("device-net", "refresh-stale");
    }

    [Fact]
    public async Task Start_NoSavedToken_LogsInWithDeviceIdOnly()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(200, LoginJson("access-start", "refresh-start", "https://sess.example.com")));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-start" };
        var auth = new AuthClient(http, store, config);

        await auth.StartAsync();

        Assert.Single(handler.Requests);
        Assert.EndsWith("/auth/anonymous", handler.Requests[0].RequestUri!.AbsolutePath);
        Assert.Equal("access-start", auth.AccessToken);
        AssertNoSecretLeaks("device-start", "access-start", "refresh-start");
    }

    [Fact]
    public async Task Start_WithSavedToken_RefreshesOnly()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(200, LoginJson("access-start", "refresh-start", "https://sess.example.com")));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-start", RefreshToken = "refresh-saved" };
        var auth = new AuthClient(http, store, config);

        await auth.StartAsync();

        Assert.Single(handler.Requests);
        Assert.EndsWith("/auth/refresh", handler.Requests[0].RequestUri!.AbsolutePath);
        Assert.Contains("\"refresh_token\":\"refresh-saved\"", handler.Requests[0].Body!);
        AssertNoSecretLeaks("device-start", "refresh-saved", "access-start", "refresh-start");
    }

    [Fact]
    public async Task Logout_SendsSavedTokenAndClearsEvenOnServerError()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(500, "{\"error\":{\"code\":\"boom\",\"message\":\"kaput\"}}"));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-logout", RefreshToken = "refresh-logout" };
        var auth = new AuthClient(http, store, config);
        var loggedOut = 0;
        auth.LoggedOut += () => loggedOut++;

        await auth.LogoutAsync();

        Assert.Single(handler.Requests);
        Assert.EndsWith("/auth/logout", handler.Requests[0].RequestUri!.AbsolutePath);
        Assert.Contains("\"refresh_token\":\"refresh-logout\"", handler.Requests[0].Body!);
        Assert.Null(store.RefreshToken);
        Assert.Equal("device-logout", store.DeviceId);
        Assert.Equal(1, loggedOut);
        AssertNoSecretLeaks("device-logout", "refresh-logout");
    }

    [Fact]
    public async Task Logout_WithoutSavedToken_SendsNothing()
    {
        var handler = new FakeHandler();
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var http = new OtomoHttp(client, config);
        var store = new MemorySecretStore { DeviceId = "device-logout" };
        var auth = new AuthClient(http, store, config);
        var loggedOut = 0;
        auth.LoggedOut += () => loggedOut++;

        await auth.LogoutAsync();

        Assert.Empty(handler.Requests);
        Assert.Null(store.RefreshToken);
        Assert.Equal("device-logout", store.DeviceId);
        Assert.Equal(1, loggedOut);
        AssertNoSecretLeaks("device-logout");
    }

    [Fact]
    public async Task Login_ResponseWithoutTokens_IsBadResponseAndNotLoggedIn()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, LoginJson("", "", null)));
        using var client = new HttpClient(handler);
        var config = NewConfig();
        var store = new MemorySecretStore { RefreshToken = "kept" };
        var auth = new AuthClient(new OtomoHttp(client, config), store, config);
        var loggedIn = 0;
        auth.LoggedIn += () => loggedIn++;

        var e = await Assert.ThrowsAsync<OtomoError>(() => auth.LoginWithDeviceIdAsync());

        Assert.Equal("bad_response", e.Code);
        Assert.False(auth.IsLoggedIn);
        Assert.Equal(0, loggedIn);
        Assert.Equal("kept", store.RefreshToken);
    }
}
