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

public sealed class PartyClientTests
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

    private static (PartyClient Client, FakeHandler Handler) MakeClient()
    {
        var handler = new FakeHandler();
        var client = new PartyClient(new OtomoHttp(new HttpClient(handler), Config()), new StubAuthClient());
        return (client, handler);
    }

    private const string SamplePartyJson =
        "{\"party_id\":\"pt1\",\"leader_id\":\"p1\",\"revision\":3,\"max_size\":4,\"state\":\"forming\"," +
        "\"settings\":{\"map\":\"any\"},\"members\":[{\"player_id\":\"p1\",\"display_name\":\"Ann\"," +
        "\"discriminator\":1,\"joined_at\":\"2026-01-01T00:00:00Z\",\"ready\":false}]}";

    [Fact]
    public async Task CreateAsync_PostsToPartyAndParsesResult()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(201, SamplePartyJson));

        var party = await client.CreateAsync();

        Assert.Equal("pt1", party.PartyId);
        Assert.Equal("p1", party.LeaderId);
        Assert.Equal(3, party.Revision);
        Assert.Equal(4, party.MaxSize);
        Assert.Equal("forming", party.State);
        Assert.Equal("any", party.Settings["map"]);
        Assert.Single(party.Members);
        Assert.Null(party.Match);
        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task CreateAsync_AlreadyInParty_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(409, "{\"error\":{\"code\":\"already_in_party\",\"message\":\"leave it first\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.CreateAsync());

        Assert.Equal(409, error.Status);
        Assert.Equal("already_in_party", error.Code);
    }

    [Fact]
    public async Task GetAsync_ParsesMatchWhenInGame()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200,
            "{\"party_id\":\"pt1\",\"leader_id\":\"p1\",\"revision\":9,\"max_size\":4,\"state\":\"in_game\"," +
            "\"settings\":{},\"members\":[]," +
            "\"match\":{\"allocation_id\":\"a1\",\"address\":\"1.2.3.4\",\"port\":7777}}"));

        var party = await client.GetAsync();

        Assert.NotNull(party.Match);
        Assert.Equal("a1", party.Match!.AllocationId);
        Assert.Equal(7777, party.Match.Port);
        Assert.Equal(HttpMethod.Get, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task GetAsync_NotInParty_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(404, "{\"error\":{\"code\":\"not_in_party\",\"message\":\"not in a party\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.GetAsync());

        Assert.Equal("not_in_party", error.Code);
    }

    [Fact]
    public async Task InviteAsync_SendsPlayerIdAndParsesInvite()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(201,
            "{\"invite_id\":\"inv1\",\"party_id\":\"pt1\",\"to_player\":\"p2\",\"expires_at\":\"2026-01-01T00:05:00Z\"}"));

        var invite = await client.InviteAsync("p2");

        Assert.Equal("inv1", invite.InviteId);
        Assert.Equal("p2", invite.ToPlayer);
        Assert.Contains("\"player_id\":\"p2\"", handler.Requests[0].Body);
        Assert.Equal("https://session.test/party/invites", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task AcceptInviteAsync_PostsToAcceptPath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200, SamplePartyJson));

        var party = await client.AcceptInviteAsync("inv1");

        Assert.Equal("pt1", party.PartyId);
        Assert.Equal("https://session.test/party/invites/inv1/accept", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task AcceptInviteAsync_Expired_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(410, "{\"error\":{\"code\":\"invite_expired\",\"message\":\"expired\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.AcceptInviteAsync("inv1"));

        Assert.Equal(410, error.Status);
        Assert.Equal("invite_expired", error.Code);
    }

    [Fact]
    public async Task DeclineInviteAsync_PostsToDeclinePath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.DeclineInviteAsync("inv1");

        Assert.Equal("https://session.test/party/invites/inv1/decline", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task LeaveAsync_PostsToLeavePath()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        await client.LeaveAsync();

        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party/leave", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task KickAsync_SendsRevisionInBody()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200, SamplePartyJson));

        await client.KickAsync("p2", 3);

        Assert.Equal("https://session.test/party/kick/p2", handler.Requests[0].RequestUri!.ToString());
        Assert.Contains("\"revision\":3", handler.Requests[0].Body);
    }

    [Fact]
    public async Task KickAsync_StaleRevision_ThrowsRevisionMismatch()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(409, "{\"error\":{\"code\":\"revision_mismatch\",\"message\":\"stale\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.KickAsync("p2", 1));

        Assert.Equal("revision_mismatch", error.Code);
    }

    [Fact]
    public async Task KickAsync_NotLeader_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(403, "{\"error\":{\"code\":\"not_leader\",\"message\":\"leader only\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.KickAsync("p2", 3));

        Assert.Equal(403, error.Status);
        Assert.Equal("not_leader", error.Code);
    }

    [Fact]
    public async Task PromoteAsync_SendsRevisionInBody()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200, SamplePartyJson));

        await client.PromoteAsync("p2", 3);

        Assert.Equal("https://session.test/party/promote/p2", handler.Requests[0].RequestUri!.ToString());
        Assert.Contains("\"revision\":3", handler.Requests[0].Body);
    }

    [Fact]
    public async Task GetAsync_EmptyBody_ThrowsBadResponse()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => new HttpResponseMessage(HttpStatusCode.NoContent));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.GetAsync());

        Assert.Equal("bad_response", error.Code);
    }

    [Fact]
    public async Task UpdateSettingsAsync_SendsPatchWithSettingsAndRevision()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200, SamplePartyJson));

        await client.UpdateSettingsAsync(new Dictionary<string, string> { ["map"] = "harbor" }, 3);

        Assert.Equal(HttpMethod.Patch, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party/settings", handler.Requests[0].RequestUri!.ToString());
        Assert.Contains("\"map\":\"harbor\"", handler.Requests[0].Body);
        Assert.Contains("\"revision\":3", handler.Requests[0].Body);
    }

    [Fact]
    public async Task UpdateSettingsAsync_InvalidSetting_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(400, "{\"error\":{\"code\":\"invalid_settings\",\"message\":\"bad key\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(
            () => client.UpdateSettingsAsync(new Dictionary<string, string> { ["nope"] = "x" }, 3));

        Assert.Equal(400, error.Status);
        Assert.Equal("invalid_settings", error.Code);
    }

    [Fact]
    public async Task SetReadyAsync_SendsReadyFlag()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200, SamplePartyJson));

        await client.SetReadyAsync(true);

        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party/ready", handler.Requests[0].RequestUri!.ToString());
        Assert.Contains("\"ready\":true", handler.Requests[0].Body);
    }

    [Fact]
    public async Task LaunchAsync_SendsRevision_ParsesParty()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(202, SamplePartyJson));

        var party = await client.LaunchAsync(3);

        Assert.Equal("pt1", party.PartyId);
        Assert.Equal("https://session.test/party/launch", handler.Requests[0].RequestUri!.ToString());
        Assert.Contains("\"revision\":3", handler.Requests[0].Body);
    }

    [Fact]
    public async Task LaunchAsync_NotReady_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(409, "{\"error\":{\"code\":\"not_ready\",\"message\":\"not everyone is ready\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.LaunchAsync(3));

        Assert.Equal("not_ready", error.Code);
    }

    [Fact]
    public async Task GetLaunchTicketAsync_ParsesTicket()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(200,
            "{\"address\":\"1.2.3.4\",\"port\":7777,\"ticket\":\"tok-1\",\"ticket_expires_at\":\"2026-01-01T00:05:00Z\"}"));

        var ticket = await client.GetLaunchTicketAsync();

        Assert.Equal("1.2.3.4", ticket.Address);
        Assert.Equal(7777, ticket.Port);
        Assert.Equal("tok-1", ticket.Ticket);
        Assert.Equal(HttpMethod.Post, handler.Requests[0].Method);
        Assert.Equal("https://session.test/party/launch/ticket", handler.Requests[0].RequestUri!.ToString());
    }

    [Fact]
    public async Task GetLaunchTicketAsync_NotInGame_ThrowsWithCode()
    {
        var (client, handler) = MakeClient();
        handler.Respond(_ => Json(409, "{\"error\":{\"code\":\"not_in_game\",\"message\":\"not in a match\"}}"));

        var error = await Assert.ThrowsAsync<OtomoError>(() => client.GetLaunchTicketAsync());

        Assert.Equal("not_in_game", error.Code);
    }
}
