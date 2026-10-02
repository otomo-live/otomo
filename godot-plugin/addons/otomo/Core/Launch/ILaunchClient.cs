#nullable enable
using System;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Launch;

/// Payload of a `party.launching` event (guide §8.2, doc 14 §7): sent to each member
/// individually, with a ticket valid for 60 seconds.
public sealed class LaunchingInfo
{
    [JsonPropertyName("allocation_id")] public string AllocationId { get; set; } = "";
    [JsonPropertyName("address")] public string Address { get; set; } = "";
    [JsonPropertyName("port")] public int Port { get; set; }
    [JsonPropertyName("ticket")] public string Ticket { get; set; } = "";
    [JsonPropertyName("ticket_expires_at")] public DateTimeOffset TicketExpiresAt { get; set; }
}

/// Payload of a `party.launch_failed` event. Reason is "no_capacity" or
/// "allocator_unavailable"; the lobby is forming again with ready flags kept.
public sealed class LaunchFailedInfo
{
    [JsonPropertyName("reason")] public string Reason { get; set; } = "";
}

/// Payload of a `party.returned` event. Reason is "ended", "expired", "server_dead" or
/// "server_restarted"; the lobby is forming again with every ready flag cleared.
public sealed class ReturnedInfo
{
    [JsonPropertyName("reason")] public string Reason { get; set; } = "";
}

/// Ties the launch lobby events (guide §8.2) to the proxy handshake (§8.4): parses the
/// three launch-related event types out of IEventsClient's raw stream and raises them
/// typed, and drives JoinAsync's single automatic retry on an expired or reused ticket
/// (§8.3-§8.4's "get a fresh ticket and start again"). Does not touch ENet or
/// SceneMultiplayer -- JoinAsync's result carries the local port for the Godot layer to
/// create the actual ENetMultiplayerPeer with, and to send the same ticket via
/// SceneMultiplayer.SendAuth when the game server asks (guide §8.4 step 4).
public interface ILaunchClient
{
    /// A game server was reserved for this player specifically. Connect now (JoinAsync);
    /// if TicketExpiresAt has already passed (e.g. the event arrived late after a resync),
    /// get a fresh ticket instead (IPartyClient.GetLaunchTicketAsync).
    event Action<LaunchingInfo>? Launching;

    /// No server was available. The lobby is forming again; show Reason and let the
    /// leader retry.
    event Action<LaunchFailedInfo>? LaunchFailed;

    /// The match ended (or the client should leave it). Return to the lobby.
    event Action<ReturnedInfo>? Returned;

    /// Runs the proxy handshake for `info`. On OTNO expired/reused, fetches one fresh
    /// ticket and retries once more with it; any other outcome (including a second
    /// refusal) is returned as-is rather than retried again.
    Task<JoinHandshakeResult> JoinAsync(LaunchingInfo info, CancellationToken ct = default);
}
