#nullable enable
using System.Collections.Generic;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Patch;

/// Step 1 of the player flow (guide §5). This interface fixes the seam and the manifest
/// contract the updater is built on.
public interface IPatchClient
{
    /// Runs the whole CHECKING → … → READY flow.
    Task<PatchResult> RunAsync(CancellationToken ct = default);
}

public enum PatchResult
{
    Ready,
    OfflineReady,
    ClientTooOld,
    /// The caller declined the planned download; the cached release is used instead.
    Declined,
}

/// GET /patch/v1/{channel}/manifest (guide §5.2).
public sealed class Manifest
{
    [JsonPropertyName("format")] public int Format { get; set; }
    [JsonPropertyName("channel")] public string Channel { get; set; } = "";
    [JsonPropertyName("release_id")] public long ReleaseId { get; set; }
    [JsonPropertyName("min_client_version")] public string MinClientVersion { get; set; } = "0.0.0";
    [JsonPropertyName("config")] public Dictionary<string, ManifestFile> Config { get; set; } = new();
    [JsonPropertyName("packs")] public List<ManifestPack> Packs { get; set; } = new();
}

public class ManifestFile
{
    [JsonPropertyName("sha256")] public string Sha256 { get; set; } = "";
    [JsonPropertyName("size")] public long Size { get; set; }
    [JsonPropertyName("version")] public int Version { get; set; }   // config only
}

public sealed class ManifestPack : ManifestFile
{
    [JsonPropertyName("name")] public string Name { get; set; } = "";
}
