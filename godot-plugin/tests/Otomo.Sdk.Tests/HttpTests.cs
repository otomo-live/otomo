#nullable enable
using System;
using System.Net;
using System.Net.Http;
using System.Text;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk;
using OtomoSdk.Http;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class HttpTests
{
    private sealed class PlayerDto
    {
        [JsonPropertyName("player_id")] public string PlayerId { get; set; } = "";
        [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
    }

    private sealed class BodyDto
    {
        [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
    }

    private static OtomoConfig Config() =>
        new("https://example.test", "/tmp/otomo") { RequestTimeout = TimeSpan.FromSeconds(5) };

    private static HttpResponseMessage Json(int status, string json) =>
        new((HttpStatusCode)status)
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        };

    private static HttpResponseMessage Html(int status, string html) =>
        new((HttpStatusCode)status)
        {
            Content = new StringContent(html, Encoding.UTF8, "text/html"),
        };

    [Fact]
    public async Task Get_DeserializesSnakeCaseAndIgnoresUnknownFields()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(200, "{\"player_id\":\"p1\",\"display_name\":\"Bob\",\"extra\":123}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var result = await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/player/me");

        Assert.NotNull(result);
        Assert.Equal("p1", result!.PlayerId);
        Assert.Equal("Bob", result.DisplayName);
    }

    [Fact]
    public async Task NoContent_ReturnsDefault()
    {
        var handler = new FakeHandler().Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var result = await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x");

        Assert.Null(result);
    }

    [Fact]
    public async Task ErrorBody_IsParsed()
    {
        var handler = new FakeHandler().Respond(
            _ => Json(400, "{\"error\":{\"code\":\"invalid\",\"message\":\"bad\",\"request_id\":\"rid-1\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(400, error.Status);
        Assert.Equal("invalid", error.Code);
        Assert.Equal("bad", error.Message);
        Assert.Equal("rid-1", error.RequestId);
    }

    [Fact]
    public async Task ErrorWithoutBodyRequestId_UsesFirstHeaderValue()
    {
        var handler = new FakeHandler().Respond(_ =>
        {
            var res = Json(500, "{\"error\":{\"code\":\"boom\",\"message\":\"kaput\"}}");
            res.Headers.Add("X-Request-Id", new[] { "first", "second" });
            return res;
        });
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal("boom", error.Code);
        Assert.Equal("first", error.RequestId);
    }

    [Fact]
    public async Task HtmlErrorBody_BecomesUnknownCode()
    {
        var handler = new FakeHandler().Respond(_ => Html(502, "<html><body>bad gateway</body></html>"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(502, error.Status);
        Assert.Equal("unknown", error.Code);
        Assert.True(error.IsRetryableLater);
    }

    [Fact]
    public async Task Unauthorized_RefreshesOnceAndRetriesWithNewToken()
    {
        var token = "old";
        var refreshCount = 0;
        var handler = new FakeHandler()
            .Respond(_ => new HttpResponseMessage(HttpStatusCode.Unauthorized))
            .Respond(_ => Json(200, "{\"player_id\":\"p1\"}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => token;
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            token = "new";
            return Task.FromResult(true);
        };

        var result = await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x");

        Assert.NotNull(result);
        Assert.Equal("p1", result!.PlayerId);
        Assert.Equal(1, refreshCount);
        Assert.Equal(2, handler.Requests.Count);
        Assert.Equal("Bearer old", handler.Requests[0].Authorization);
        Assert.Equal("Bearer new", handler.Requests[1].Authorization);
    }

    [Fact]
    public async Task UnauthorizedAfterRefreshAgain_RaisesAuthenticationLostOnce()
    {
        var refreshCount = 0;
        var lost = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(401, "{\"error\":{\"code\":\"expired\",\"message\":\"nope\",\"request_id\":\"r1\"}}"))
            .Respond(_ => Json(401, "{\"error\":{\"code\":\"expired\",\"message\":\"nope\",\"request_id\":\"r2\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "t";
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            return Task.FromResult(true);
        };
        http.AuthenticationLost += _ => lost++;

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(401, error.Status);
        Assert.Equal(1, refreshCount);
        Assert.Equal(1, lost);
        Assert.Equal(2, handler.Requests.Count);
    }

    [Fact]
    public async Task Unauthorized_RefreshFalse_ThrowsWithoutRetry()
    {
        var refreshCount = 0;
        var lost = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(401, "{\"error\":{\"code\":\"expired\",\"message\":\"nope\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "t";
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            return Task.FromResult(false);
        };
        http.AuthenticationLost += _ => lost++;

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(401, error.Status);
        Assert.Equal(1, refreshCount);
        Assert.Equal(1, lost);
        Assert.Single(handler.Requests);
    }

    [Fact]
    public async Task ReleaseOutdated_RePatchesOnceAndRetries()
    {
        var releaseRefreshCount = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(409, "{\"error\":{\"code\":\"release_outdated\",\"message\":\"old\"}}"))
            .Respond(_ => Json(200, "{\"player_id\":\"p1\"}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.RefreshRelease = _ =>
        {
            releaseRefreshCount++;
            return Task.FromResult(true);
        };

        var result = await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x");

        Assert.NotNull(result);
        Assert.Equal("p1", result!.PlayerId);
        Assert.Equal(1, releaseRefreshCount);
        Assert.Equal(2, handler.Requests.Count);
    }

    [Fact]
    public async Task ReleaseOutdated_RefreshFalse_ThrowsWithoutRetry()
    {
        var releaseRefreshCount = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(409, "{\"error\":{\"code\":\"release_outdated\",\"message\":\"old\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.RefreshRelease = _ =>
        {
            releaseRefreshCount++;
            return Task.FromResult(false);
        };

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(409, error.Status);
        Assert.Equal("release_outdated", error.Code);
        Assert.Equal(1, releaseRefreshCount);
        Assert.Single(handler.Requests);
    }

    [Fact]
    public async Task ReleaseOutdatedAgainAfterRefresh_DoesNotLoopAndThrowsSecondError()
    {
        var releaseRefreshCount = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(409, "{\"error\":{\"code\":\"release_outdated\",\"message\":\"old\",\"request_id\":\"r1\"}}"))
            .Respond(_ => Json(409, "{\"error\":{\"code\":\"release_outdated\",\"message\":\"old\",\"request_id\":\"r2\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.RefreshRelease = _ =>
        {
            releaseRefreshCount++;
            return Task.FromResult(true);
        };

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(409, error.Status);
        Assert.Equal("r2", error.RequestId);
        Assert.Equal(1, releaseRefreshCount);
        Assert.Equal(2, handler.Requests.Count);
    }

    [Fact]
    public async Task OtherConflict_NeverTriggersReleaseRefresh()
    {
        var releaseRefreshCount = 0;
        var handler = new FakeHandler()
            .Respond(_ => Json(409, "{\"error\":{\"code\":\"revision_mismatch\",\"message\":\"stale\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.RefreshRelease = _ =>
        {
            releaseRefreshCount++;
            return Task.FromResult(true);
        };

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(409, error.Status);
        Assert.Equal("revision_mismatch", error.Code);
        Assert.Equal(0, releaseRefreshCount);
        Assert.Single(handler.Requests);
    }

    [Fact]
    public async Task Forbidden_NeverRefreshes()
    {
        var refreshCount = 0;
        var handler = new FakeHandler().Respond(_ => Json(403, "{\"error\":{\"code\":\"forbidden\",\"message\":\"no\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "t";
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            return Task.FromResult(true);
        };

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(403, error.Status);
        Assert.Equal(0, refreshCount);
        Assert.Single(handler.Requests);
    }

    [Fact]
    public async Task Unauthorized_WhenNotAuthenticated_NeverRefreshes()
    {
        var refreshCount = 0;
        var handler = new FakeHandler().Respond(_ => Json(401, "{\"error\":{\"code\":\"expired\",\"message\":\"no\"}}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.GetAccessToken = () => "t";
        http.RefreshAccessToken = _ =>
        {
            refreshCount++;
            return Task.FromResult(true);
        };

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x", authenticated: false));

        Assert.Equal(401, error.Status);
        Assert.Equal(0, refreshCount);
        Assert.Null(handler.Requests[0].Authorization);
    }

    [Fact]
    public async Task HttpRequestException_BecomesNetworkError()
    {
        var handler = new FakeHandler().Respond(_ => throw new HttpRequestException("dns failed"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x"));

        Assert.Equal(0, error.Status);
        Assert.Equal("network", error.Code);
    }

    [Fact]
    public async Task Timeout_BecomesTimeoutError()
    {
        var handler = new FakeHandler { Delay = TimeSpan.FromMilliseconds(500) }
            .Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x", timeout: TimeSpan.FromMilliseconds(50)));

        Assert.Equal(0, error.Status);
        Assert.Equal("timeout", error.Code);
    }

    [Fact]
    public async Task CallerCancellation_PropagatesOperationCanceled()
    {
        var handler = new FakeHandler { Delay = TimeSpan.FromMilliseconds(500) }
            .Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        using var cts = new CancellationTokenSource();
        cts.Cancel();

        await Assert.ThrowsAnyAsync<OperationCanceledException>(
            () => http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x", ct: cts.Token));
    }

    [Fact]
    public async Task AbsoluteUrl_IsUsedAsIs()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        await http.SendAsync<PlayerDto>(HttpMethod.Get, "https://other.test/path?x=1");

        Assert.Equal("https://other.test/path?x=1", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task RelativePath_IsPrefixedWithBaseUrl()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/player/me");

        Assert.Equal("https://example.test/api/player/me", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task ExtraHeaders_AreSent_AndNoAuthorizationWhenTokenNull()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());
        http.ExtraHeaders["X-Release-Id"] = "rel-7";

        await http.SendAsync<PlayerDto>(HttpMethod.Get, "/api/x");

        var request = handler.Requests[0];
        Assert.Equal("rel-7", request.Headers["X-Release-Id"]);
        Assert.Null(request.Authorization);
    }

    [Fact]
    public async Task Post_BodyIsSerializedAsJson()
    {
        var handler = new FakeHandler().Respond(_ => Json(200, "{}"));
        using var client = new HttpClient(handler);
        var http = new OtomoHttp(client, Config());

        await http.SendAsync(HttpMethod.Post, "/api/x", new BodyDto { DisplayName = "Bob" });

        var request = handler.Requests[0];
        Assert.StartsWith("application/json", request.Headers["Content-Type"]);
        Assert.Contains("\"display_name\":\"Bob\"", request.Body!);
    }
}
