#nullable enable
using System;

namespace OtomoSdk;

/// Everything the SDK needs to know about its deployment (guide §3.4). The Godot layer
/// fills it from Project Settings; tests build it directly. Core code never touches Godot.
public sealed class OtomoConfig
{
    /// Gateway base URL, e.g. "https://play.example.com". No trailing slash is kept.
    public string BaseUrl { get; }

    /// Patch channel. Players always use "live".
    public string Channel { get; init; } = "live";

    /// Version of the game executable (major.minor.patch), compared with min_client_version.
    public string ClientVersion { get; init; } = "0.0.0";

    /// OS path of the SDK's data folder (user://otomo globalized). Holds device_id,
    /// refresh_token and patch/.
    public string DataDir { get; }

    /// Timeout for ordinary calls (guide §3.2 rule 4).
    public TimeSpan RequestTimeout { get; init; } = TimeSpan.FromSeconds(15);

    /// Timeout for the Session events long-poll (guide §7.3).
    public TimeSpan LongPollTimeout { get; init; } = TimeSpan.FromSeconds(35);

    /// Blob downloads (guide §5) hold the connection open for a large file, so
    /// RequestTimeout only bounds getting the headers. This bounds silence in between
    /// reads of the body -- a connection that stops delivering bytes without dropping,
    /// which RequestTimeout alone would never catch.
    public TimeSpan StallTimeout { get; init; } = TimeSpan.FromSeconds(20);

    /// Diagnostic sink. Never receives secrets (device ID, tokens). Null = silent.
    public Action<string>? Log { get; init; }

    public OtomoConfig(string baseUrl, string dataDir)
    {
        if (string.IsNullOrWhiteSpace(baseUrl)) throw new ArgumentException("base URL is required", nameof(baseUrl));
        if (!Uri.TryCreate(baseUrl.Trim(), UriKind.Absolute, out var uri) || (uri.Scheme != "https" && uri.Scheme != "http"))
            throw new ArgumentException($"base URL must be an absolute http(s) URL: {baseUrl}", nameof(baseUrl));
        if (string.IsNullOrWhiteSpace(dataDir)) throw new ArgumentException("data dir is required", nameof(dataDir));
        BaseUrl = baseUrl.Trim().TrimEnd('/');
        DataDir = dataDir;
    }
}
