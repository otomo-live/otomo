#nullable enable
using System;
using System.Net.Http;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Http;

namespace OtomoSdk.Auth;

/// <summary>
/// Implements the player auth flow (guide §6). The access token lives only in memory; the
/// refresh token is persisted through <see cref="ISecretStore"/>. Wires itself into
/// <see cref="OtomoHttp"/> so authenticated calls can refresh once. Never logs secrets.
/// </summary>
public sealed class AuthClient : IAuthClient
{
    private readonly OtomoHttp _http;
    private readonly ISecretStore _store;
    private readonly OtomoConfig _config;
    private readonly SemaphoreSlim _gate = new SemaphoreSlim(1, 1);

    private string? _accessToken;
    private string _sessionBaseUrl;

    public AuthClient(OtomoHttp http, ISecretStore store, OtomoConfig config)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _store = store ?? throw new ArgumentNullException(nameof(store));
        _config = config ?? throw new ArgumentNullException(nameof(config));
        _sessionBaseUrl = config.BaseUrl + "/api/player/session";

        http.GetAccessToken = () => AccessToken;
        http.RefreshAccessToken = RefreshAccessTokenAsync;
    }

    public bool IsLoggedIn => _accessToken is not null;

    public string? AccessToken => _accessToken;

    public string SessionBaseUrl => _sessionBaseUrl;

    public event Action? LoggedIn;

    public event Action? LoggedOut;

    public async Task LoginWithDeviceIdAsync(CancellationToken ct = default)
    {
        await _gate.WaitAsync(ct);
        try
        {
            await LoginCoreAsync(ct);
        }
        finally
        {
            _gate.Release();
        }
    }

    public async Task<bool> RefreshAccessTokenAsync(CancellationToken ct = default)
    {
        var tokenBefore = _accessToken;

        await _gate.WaitAsync(ct);
        try
        {
            // Another caller refreshed while we waited: its token is already in place.
            if (_accessToken is not null && _accessToken != tokenBefore)
                return true;

            var refreshToken = _store.LoadRefreshToken();
            if (string.IsNullOrEmpty(refreshToken))
                return await TryDeviceLoginAsync(ct);

            try
            {
                var response = await _http.SendAsync<LoginResponse>(
                    HttpMethod.Post,
                    "/auth/refresh",
                    new RefreshTokenBody { RefreshToken = refreshToken },
                    authenticated: false,
                    ct: ct);

                if (response is null)
                    throw new OtomoError(200, "bad_response", "empty refresh response", "");

                ApplyLogin(response);
                return true;
            }
            catch (OtomoError e) when (e.Status == 401)
            {
                _config.Log?.Invoke("otomo auth: refresh rejected, logging in with device id");
                _store.ClearRefreshToken();
                _accessToken = null;
                return await TryDeviceLoginAsync(ct);
            }
            catch (OtomoError)
            {
                // Network, 5xx, 429, … Keep the saved refresh token for a later retry.
                if (_accessToken is null)
                    LoggedOut?.Invoke();
                return false;
            }
        }
        finally
        {
            _gate.Release();
        }
    }

    public async Task StartAsync(CancellationToken ct = default)
    {
        if (_store.LoadRefreshToken() is not null && await RefreshAccessTokenAsync(ct))
            return;

        await LoginWithDeviceIdAsync(ct);
    }

    public async Task LogoutAsync(CancellationToken ct = default)
    {
        var refreshToken = _store.LoadRefreshToken();
        try
        {
            if (refreshToken is not null)
            {
                try
                {
                    await _http.SendAsync(
                        HttpMethod.Post,
                        "/auth/logout",
                        new RefreshTokenBody { RefreshToken = refreshToken },
                        authenticated: false,
                        ct: ct);
                }
                catch (OtomoError)
                {
                    // Logout always succeeds locally.
                }
            }
        }
        finally
        {
            _store.ClearRefreshToken();
            _accessToken = null;
            _config.Log?.Invoke("otomo auth: logged out");
            LoggedOut?.Invoke();
        }
    }

    private async Task LoginCoreAsync(CancellationToken ct)
    {
        var deviceId = _store.LoadOrCreateDeviceId();
        var response = await _http.SendAsync<LoginResponse>(
            HttpMethod.Post,
            "/auth/anonymous",
            new DeviceIdBody { DeviceId = deviceId },
            authenticated: false,
            ct: ct);

        if (response is null)
            throw new OtomoError(200, "bad_response", "empty login response", "");

        ApplyLogin(response);
    }

    private async Task<bool> TryDeviceLoginAsync(CancellationToken ct)
    {
        try
        {
            await LoginCoreAsync(ct);
            return true;
        }
        catch (OtomoError)
        {
            if (_accessToken is null)
                LoggedOut?.Invoke();
            return false;
        }
    }

    private void ApplyLogin(LoginResponse response)
    {
        if (string.IsNullOrEmpty(response.AccessToken) || string.IsNullOrEmpty(response.RefreshToken))
            throw new OtomoError(200, "bad_response", "login response without tokens", "");

        // Persist the new refresh token before publishing the access token: the old one is
        // already spent, so this file must never keep it.
        _store.SaveRefreshToken(response.RefreshToken);

        _accessToken = response.AccessToken;
        _sessionBaseUrl = ResolveSessionBaseUrl(response.Services?.Session);
        _config.Log?.Invoke("otomo auth: logged in");
        LoggedIn?.Invoke();
    }

    private string ResolveSessionBaseUrl(string? session)
    {
        if (!string.IsNullOrWhiteSpace(session) &&
            Uri.TryCreate(session.Trim(), UriKind.Absolute, out var uri) &&
            (uri.Scheme == Uri.UriSchemeHttp || uri.Scheme == Uri.UriSchemeHttps))
        {
            return session.Trim().TrimEnd('/');
        }

        return _config.BaseUrl + "/api/player/session";
    }

    private sealed class DeviceIdBody
    {
        [JsonPropertyName("device_id")] public string DeviceId { get; set; } = "";
    }

    private sealed class RefreshTokenBody
    {
        [JsonPropertyName("refresh_token")] public string RefreshToken { get; set; } = "";
    }
}
