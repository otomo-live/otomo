#nullable enable
using System;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Auth;

/// Step 2 of the player flow (guide §6). Owns the access token (memory only) and the
/// refresh token (via ISecretStore).
public interface IAuthClient
{
    /// True while an access token is held.
    bool IsLoggedIn { get; }

    /// Current access token, or null. Read by OtomoHttp for "Authorization: Bearer".
    string? AccessToken { get; }

    /// Session base URL from the last login's services.session, else
    /// "{BaseUrl}/api/player/session" (guide §6.2).
    string SessionBaseUrl { get; }

    /// POST /auth/anonymous with the device ID (created on first use). Throws OtomoError.
    Task LoginWithDeviceIdAsync(CancellationToken ct = default);

    /// POST /auth/refresh with the saved refresh token. Single-flight. On 401 from refresh:
    /// clears the saved token and logs in with the device ID instead. Returns false when no
    /// usable login could be obtained (never throws OtomoError). This is the OtomoHttp hook.
    Task<bool> RefreshAccessTokenAsync(CancellationToken ct = default);

    /// Startup: refresh if a refresh token is saved, else (or if that fails) device login.
    /// Throws OtomoError when neither worked.
    Task StartAsync(CancellationToken ct = default);

    /// POST /auth/logout, then forget both tokens whatever the answer. Keeps the device ID.
    Task LogoutAsync(CancellationToken ct = default);

    /// Raised after every successful login or refresh.
    event Action? LoggedIn;

    /// Raised when the tokens are dropped (logout, or login lost and could not be recovered).
    event Action? LoggedOut;
}

/// Body of POST /auth/anonymous and /auth/refresh (guide §6.2).
public sealed class LoginResponse
{
    [JsonPropertyName("schema_version")] public int SchemaVersion { get; set; }
    [JsonPropertyName("access_token")] public string AccessToken { get; set; } = "";
    [JsonPropertyName("expires_in")] public int ExpiresIn { get; set; }
    [JsonPropertyName("refresh_token")] public string RefreshToken { get; set; } = "";
    [JsonPropertyName("services")] public ServicesPayload? Services { get; set; }
}

public sealed class ServicesPayload
{
    [JsonPropertyName("session")] public string? Session { get; set; }
    [JsonPropertyName("match")] public string? Match { get; set; }
}
