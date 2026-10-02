#nullable enable
using System;
using System.Collections.Generic;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Session;

/// One friend, or one pending request in either direction (guide §7.4).
public sealed class Friend
{
    [JsonPropertyName("player_id")] public string PlayerId { get; set; } = "";
    [JsonPropertyName("display_name")] public string DisplayName { get; set; } = "";
    [JsonPropertyName("discriminator")] public int Discriminator { get; set; }

    /// Set on an accepted friend (when the friendship began); omitted on a pending request.
    [JsonPropertyName("since")] public DateTimeOffset? Since { get; set; }

    /// The friend's presence (guide §7.2). Only GET /friends fills this in, and only for
    /// entries in Friends (not Incoming/Outgoing), and only when the server could read it.
    [JsonPropertyName("status")] public string? Status { get; set; }

    /// "pending", or "accepted" when they had already asked you. Set on the answer to
    /// RequestAsync/AcceptAsync; null on GET /friends' own entries (state is implied by
    /// which list the entry is in).
    [JsonPropertyName("state")] public string? State { get; set; }

    public override string ToString() => $"{DisplayName}#{Discriminator:D4}";
}

/// GET /friends' answer: accepted friends plus requests in both directions.
public sealed class FriendsList
{
    [JsonPropertyName("friends")] public List<Friend> Friends { get; set; } = new();
    [JsonPropertyName("incoming")] public List<Friend> Incoming { get; set; } = new();
    [JsonPropertyName("outgoing")] public List<Friend> Outgoing { get; set; } = new();
}

/// Friends and blocks (guide §7.4). Events friend.request/friend.accepted/friend.removed
/// (IEventsClient) are hints that one of these lists changed; re-fetch with ListAsync to
/// see the actual state.
public interface IFriendsClient
{
    /// GET /friends.
    Task<FriendsList> ListAsync(CancellationToken ct = default);

    /// POST /friends/requests. A button call, not a background loop, so the guide's §4.5
    /// back-off does not apply: let the failure surface. Throws OtomoError player_not_found
    /// (404, no one with that name/number), blocked (403), already_friends or friend_limit
    /// (409), or rate_limit_exceeded (429, with Retry-After -- at most 20 requests/hour).
    Task<Friend> RequestAsync(string displayName, int discriminator, CancellationToken ct = default);

    /// POST /friends/requests/{playerId}/accept.
    Task<Friend> AcceptAsync(string playerId, CancellationToken ct = default);

    /// POST /friends/requests/{playerId}/decline.
    Task DeclineAsync(string playerId, CancellationToken ct = default);

    /// DELETE /friends/{playerId}: ends a friendship, or withdraws/refuses a pending
    /// request in either direction.
    Task RemoveAsync(string playerId, CancellationToken ct = default);

    /// POST /blocks/{playerId}. Also ends any friendship and party invites between you,
    /// in the same server-side transaction.
    Task BlockAsync(string playerId, CancellationToken ct = default);

    /// DELETE /blocks/{playerId}. Unblocking does not restore a friendship blocking ended.
    Task UnblockAsync(string playerId, CancellationToken ct = default);
}
