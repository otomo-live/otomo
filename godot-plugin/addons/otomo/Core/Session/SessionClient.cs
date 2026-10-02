#nullable enable
using System;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;

namespace OtomoSdk.Session;

/// <summary>
/// First Session calls (guide §7). Behind the gateway they need the bearer token that
/// <see cref="OtomoHttp"/> adds; today they answer 501 until the server ships them.
/// </summary>
public sealed class SessionClient : ISessionClient
{
    private readonly OtomoHttp _http;
    private readonly IAuthClient _auth;

    public SessionClient(OtomoHttp http, IAuthClient auth)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _auth = auth ?? throw new ArgumentNullException(nameof(auth));
    }

    public Task<PlayerProfile> InitAsync(CancellationToken ct = default) =>
        SendAsync(HttpMethod.Post, "/me/init", ct);

    public Task<PlayerProfile> GetMeAsync(CancellationToken ct = default) =>
        SendAsync(HttpMethod.Get, "/me", ct);

    private async Task<PlayerProfile> SendAsync(HttpMethod method, string path, CancellationToken ct)
    {
        var profile = await _http.SendAsync<PlayerProfile>(method, _auth.SessionBaseUrl + path, ct: ct);
        if (profile is null)
            throw new OtomoError(200, "bad_response", "empty session response", "");

        return profile;
    }
}
