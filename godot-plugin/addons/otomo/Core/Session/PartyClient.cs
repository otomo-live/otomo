#nullable enable
using System;
using System.Collections.Generic;
using System.Net.Http;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// <inheritdoc cref="IPartyClient"/>
public sealed class PartyClient : IPartyClient
{
    private readonly OtomoHttp _http;
    private readonly IAuthClient _auth;

    public PartyClient(OtomoHttp http, IAuthClient auth)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _auth = auth ?? throw new ArgumentNullException(nameof(auth));
    }

    public Task<Party> CreateAsync(CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, "/party", null, ct);

    public Task<Party> GetAsync(CancellationToken ct = default) =>
        SendAsync(HttpMethod.Get, "/party", null, ct);

    public async Task<PartyInvite> InviteAsync(string playerId, CancellationToken ct = default)
    {
        var body = new InviteBody { PlayerId = playerId };
        var invite = await _http.SendAsync<PartyInvite>(
            HttpMethod.Post, _auth.SessionBaseUrl + "/party/invites", body, ct: ct);
        return invite ?? throw EmptyResponse();
    }

    public Task<Party> AcceptInviteAsync(string inviteId, CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, $"/party/invites/{inviteId}/accept", null, ct);

    public Task DeclineInviteAsync(string inviteId, CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Post, _auth.SessionBaseUrl + $"/party/invites/{inviteId}/decline", ct: ct);

    public Task LeaveAsync(CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Post, _auth.SessionBaseUrl + "/party/leave", ct: ct);

    public Task<Party> KickAsync(string playerId, int revision, CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, $"/party/kick/{playerId}", new RevisionBody { Revision = revision }, ct);

    public Task<Party> PromoteAsync(string playerId, int revision, CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, $"/party/promote/{playerId}", new RevisionBody { Revision = revision }, ct);

    public Task<Party> UpdateSettingsAsync(
        IReadOnlyDictionary<string, string> settings, int revision, CancellationToken ct = default) =>
        SendAsync(
            HttpMethod.Patch, "/party/settings",
            new SettingsBody { Settings = new Dictionary<string, string>(settings), Revision = revision }, ct);

    public Task<Party> SetReadyAsync(bool ready, CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, "/party/ready", new ReadyBody { Ready = ready }, ct);

    public Task<Party> LaunchAsync(int revision, CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, "/party/launch", new RevisionBody { Revision = revision }, ct);

    public async Task<LaunchTicket> GetLaunchTicketAsync(CancellationToken ct = default)
    {
        var ticket = await _http.SendAsync<LaunchTicket>(
            HttpMethod.Post, _auth.SessionBaseUrl + "/party/launch/ticket", ct: ct);
        return ticket ?? throw EmptyResponse();
    }

    private async Task<Party> SendAsync(HttpMethod method, string path, object? body, CancellationToken ct)
    {
        var party = await _http.SendAsync<Party>(method, _auth.SessionBaseUrl + path, body, ct: ct);
        return party ?? throw EmptyResponse();
    }

    private static OtomoError EmptyResponse() =>
        new(200, "bad_response", "empty party response", "");

    private sealed class InviteBody
    {
        [JsonPropertyName("player_id")] public string PlayerId { get; set; } = "";
    }

    private sealed class RevisionBody
    {
        [JsonPropertyName("revision")] public int Revision { get; set; }
    }

    private sealed class SettingsBody
    {
        [JsonPropertyName("settings")] public Dictionary<string, string> Settings { get; set; } = new();
        [JsonPropertyName("revision")] public int Revision { get; set; }
    }

    private sealed class ReadyBody
    {
        [JsonPropertyName("ready")] public bool Ready { get; set; }
    }
}
