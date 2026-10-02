#nullable enable
using System;
using System.Net.Http;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// <inheritdoc cref="IFriendsClient"/>
public sealed class FriendsClient : IFriendsClient
{
    private readonly OtomoHttp _http;
    private readonly IAuthClient _auth;

    public FriendsClient(OtomoHttp http, IAuthClient auth)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _auth = auth ?? throw new ArgumentNullException(nameof(auth));
    }

    public async Task<FriendsList> ListAsync(CancellationToken ct = default)
    {
        var list = await _http.SendAsync<FriendsList>(HttpMethod.Get, _auth.SessionBaseUrl + "/friends", ct: ct);
        return list ?? new FriendsList();
    }

    public async Task<Friend> RequestAsync(string displayName, int discriminator, CancellationToken ct = default)
    {
        var body = new FriendRequestBody { DisplayName = displayName, Discriminator = discriminator };
        var friend = await _http.SendAsync<Friend>(
            HttpMethod.Post, _auth.SessionBaseUrl + "/friends/requests", body, ct: ct);
        return friend ?? throw EmptyResponse();
    }

    public async Task<Friend> AcceptAsync(string playerId, CancellationToken ct = default)
    {
        var friend = await _http.SendAsync<Friend>(
            HttpMethod.Post, _auth.SessionBaseUrl + $"/friends/requests/{playerId}/accept", ct: ct);
        return friend ?? throw EmptyResponse();
    }

    public Task DeclineAsync(string playerId, CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Post, _auth.SessionBaseUrl + $"/friends/requests/{playerId}/decline", ct: ct);

    public Task RemoveAsync(string playerId, CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Delete, _auth.SessionBaseUrl + $"/friends/{playerId}", ct: ct);

    public Task BlockAsync(string playerId, CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Post, _auth.SessionBaseUrl + $"/blocks/{playerId}", ct: ct);

    public Task UnblockAsync(string playerId, CancellationToken ct = default) =>
        _http.SendAsync(HttpMethod.Delete, _auth.SessionBaseUrl + $"/blocks/{playerId}", ct: ct);

    private static OtomoError EmptyResponse() =>
        new(200, "bad_response", "empty friends response", "");

    private sealed class FriendRequestBody
    {
        [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
        [JsonPropertyName("discriminator")] public int Discriminator { get; set; }
    }
}
