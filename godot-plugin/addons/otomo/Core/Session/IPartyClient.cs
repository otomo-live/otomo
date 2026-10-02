#nullable enable
using System;
using System.Collections.Generic;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Session;

/// One member of a party (guide §7.4). Status is presence and is only filled in by
/// GET /party, same as Friend.Status.
public sealed class PartyMember
{
    [JsonPropertyName("player_id")] public string PlayerId { get; set; } = "";
    [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
    [JsonPropertyName("discriminator")] public int Discriminator { get; set; }
    [JsonPropertyName("joined_at")] public DateTimeOffset JoinedAt { get; set; }
    [JsonPropertyName("ready")] public bool Ready { get; set; }
    [JsonPropertyName("status")] public string? Status { get; set; }
}

/// Where to connect once the party is in_game (doc 14 §2, guide §8). Never a ticket by
/// itself -- launch (§8) is a later chunk.
public sealed class PartyMatch
{
    [JsonPropertyName("allocation_id")] public string AllocationId { get; set; } = "";
    [JsonPropertyName("address")] public string Address { get; set; } = "";
    [JsonPropertyName("port")] public int Port { get; set; }
}

/// The party, which doubles as the lobby (guide §7.4-§7.5, doc 14 §2). Settings and State
/// belong to the lobby; every party call that leaves the caller in a party answers with
/// this shape, so acting on the result (e.g. showing the new Revision) never needs a
/// separate GetAsync.
public sealed class Party
{
    [JsonPropertyName("party_id")] public string PartyId { get; set; } = "";
    [JsonPropertyName("leader_id")] public string LeaderId { get; set; } = "";
    [JsonPropertyName("revision")] public int Revision { get; set; }
    [JsonPropertyName("max_size")] public int MaxSize { get; set; }
    [JsonPropertyName("state")] public string State { get; set; } = "";
    [JsonPropertyName("settings")] public Dictionary<string, string> Settings { get; set; } = new();
    [JsonPropertyName("members")] public List<PartyMember> Members { get; set; } = new();
    [JsonPropertyName("match")] public PartyMatch? Match { get; set; }
}

/// An invite to join a party (guide §7.4). Invites last 5 minutes; accepting or declining
/// one after that answers 410 invite_expired.
public sealed class PartyInvite
{
    [JsonPropertyName("invite_id")] public string InviteId { get; set; } = "";
    [JsonPropertyName("party_id")] public string PartyId { get; set; } = "";
    [JsonPropertyName("to_player")] public string ToPlayer { get; set; } = "";
    [JsonPropertyName("expires_at")] public DateTimeOffset ExpiresAt { get; set; }
}

/// A fresh join ticket for the caller's own current match (guide §8.3). Session never
/// stores tickets -- this is how a client that missed party.launching, reconnected after
/// a resync, crashed, or holds an expired ticket gets a usable one again.
public sealed class LaunchTicket
{
    [JsonPropertyName("address")] public string Address { get; set; } = "";
    [JsonPropertyName("port")] public int Port { get; set; }
    [JsonPropertyName("ticket")] public string Ticket { get; set; } = "";
    [JsonPropertyName("ticket_expires_at")] public DateTimeOffset TicketExpiresAt { get; set; }
}

/// Party calls (guide §7.4). "Leader calls carry the revision": KickAsync and PromoteAsync
/// send the Revision of the Party the leader is looking at, and a stale one answers 409
/// revision_mismatch -- the caller should GetAsync and let the leader try again.
public interface IPartyClient
{
    /// POST /party. Throws already_in_party (409) if already in one -- leave it first.
    Task<Party> CreateAsync(CancellationToken ct = default);

    /// GET /party. Throws not_in_party (404) if not currently in one.
    Task<Party> GetAsync(CancellationToken ct = default);

    /// POST /party/invites. Any member may invite. Throws party_full (409), blocked (403),
    /// player_not_found (404).
    Task<PartyInvite> InviteAsync(string playerId, CancellationToken ct = default);

    /// POST /party/invites/{inviteId}/accept. Throws invite_expired (410, after 5 minutes),
    /// invite_not_found (404), party_full (409), blocked (403), already_in_party (409).
    Task<Party> AcceptInviteAsync(string inviteId, CancellationToken ct = default);

    /// POST /party/invites/{inviteId}/decline.
    Task DeclineInviteAsync(string inviteId, CancellationToken ct = default);

    /// POST /party/leave. If you were leader, the longest-standing remaining member
    /// becomes leader.
    Task LeaveAsync(CancellationToken ct = default);

    /// POST /party/kick/{playerId}. Leader only; throws not_leader (403),
    /// revision_mismatch (409), not_a_member (404).
    Task<Party> KickAsync(string playerId, int revision, CancellationToken ct = default);

    /// POST /party/promote/{playerId}. Leader only; same errors as KickAsync.
    Task<Party> PromoteAsync(string playerId, int revision, CancellationToken ct = default);

    /// PATCH /party/settings (doc 14 §2.1, guide §7.5). Leader only, party must be
    /// forming; every key must be a listed lobby setting with an allowed value (guide's
    /// session.rules) or this throws invalid_settings (400). A change clears every
    /// member's ready flag. Keys left out of `settings` keep their current value.
    Task<Party> UpdateSettingsAsync(
        IReadOnlyDictionary<string, string> settings, int revision, CancellationToken ct = default);

    /// POST /party/ready {"ready": true|false}. Any member, party must be forming.
    Task<Party> SetReadyAsync(bool ready, CancellationToken ct = default);

    /// POST /party/launch (doc 14 §2.1, §3, guide §8.2-§8.3). Leader only, every member
    /// ready, forming. Answers 202 once launching is committed server-side; the actual
    /// allocation happens afterwards and reaches every member as a party.launching or
    /// party.launch_failed event (IEventsClient), not as this call's result. Throws
    /// not_ready (409) if not every member is ready, invalid_settings (409) if the lobby
    /// settings are no longer allowed by the time launch was pressed, plus the usual
    /// leader-call errors (not_leader, revision_mismatch).
    Task<Party> LaunchAsync(int revision, CancellationToken ct = default);

    /// POST /party/launch/ticket. Throws not_in_party (404), not_in_game (409, the party
    /// isn't in a match), match_ended (409), not_in_match (404, you were removed from the
    /// allocation), allocator_unavailable (503, try again).
    Task<LaunchTicket> GetLaunchTicketAsync(CancellationToken ct = default);
}
