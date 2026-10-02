#nullable enable

namespace OtomoSdk.Auth;

/// Persistence for the two player secrets (guide §3.5, §6.1, §6.3). Implementations must
/// never log the values.
public interface ISecretStore
{
    /// The saved device ID, or a new one (32 CSPRNG bytes, base64url, no padding, 43 chars)
    /// written before returning.
    string LoadOrCreateDeviceId();

    string? LoadRefreshToken();
    void SaveRefreshToken(string token);
    void ClearRefreshToken();
}
