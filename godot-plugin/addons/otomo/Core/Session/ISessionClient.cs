#nullable enable
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Session;

/// Step 3 of the player flow (guide §7). PLANNED on the server (routes answer 501 today);
/// the foundation provides the seam and the first call only.
public interface ISessionClient
{
    /// POST {session}/me/init: creates the profile on first login, else returns it.
    Task<PlayerProfile> InitAsync(CancellationToken ct = default);

    /// GET {session}/me.
    Task<PlayerProfile> GetMeAsync(CancellationToken ct = default);
}

/// Proposed shape (guide §7.1); may change while Session is PLANNED.
public sealed class PlayerProfile
{
    [JsonPropertyName("player_id")] public string PlayerId { get; set; } = "";
    [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
    [JsonPropertyName("discriminator")] public int Discriminator { get; set; }

    public override string ToString() => $"{DisplayName}#{Discriminator:D4}";
}
