// Friends and blocks (SE-5; design/04-session-minimal.md SES-C1 to SES-C4).
//
// A friendship is one row, lower id first (player_lo < player_hi), in state pending or
// accepted; requested_by says who asked. Every change first locks both players'
// profile rows, lower id first. That one lock order serialises all friend changes
// between the same two players, so two players asking each other at the same moment end
// as one accepted friendship, and the friend limit cannot be overrun by two accepts at
// once. As with parties, a change returns the notices it owes and the caller publishes
// them after commit.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// Errors a friend change can end in. Each maps to one HTTP answer.
var (
	ErrAlreadyFriends  = errors.New("store: the players are already friends")
	ErrFriendLimit     = errors.New("store: a player has reached the friend limit")
	ErrRequestNotFound = errors.New("store: no such friend request")
	ErrNotFriends      = errors.New("store: the players are not friends")
)

// Friendship states.
const (
	FriendPending  = "pending"
	FriendAccepted = "accepted"
)

// Friend is one entry in a player's friends list: the other player, with their public
// name, and the friendship's state and who asked.
type Friend struct {
	PlayerID      string
	DisplayName   string
	Discriminator int
	State         string
	RequestedBy   string
	Since         time.Time
}

// pair orders two ids the way friendship stores them.
func pair(a, b string) (lo, hi string) {
	if a < b {
		return a, b
	}
	return b, a
}

// lockPlayers locks the profile rows of a and b, lower id first, and returns the
// profiles. It is ErrProfileNotFound when a has none (the caller has not called
// /me/init) and ErrPlayerNotFound when b has none.
func lockPlayers(ctx context.Context, tx pgx.Tx, a, b string) (pa, pb Friend, err error) {
	rows, err := tx.Query(ctx,
		`SELECT player_id::text, display_name, discriminator FROM player_profile
		  WHERE player_id IN ($1, $2) ORDER BY player_id FOR UPDATE`, a, b)
	if err != nil {
		return pa, pb, fmt.Errorf("store: lock players: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.PlayerID, &f.DisplayName, &f.Discriminator); err != nil {
			return pa, pb, fmt.Errorf("store: read player: %w", err)
		}
		switch f.PlayerID {
		case a:
			pa = f
		case b:
			pb = f
		}
	}
	if err := rows.Err(); err != nil {
		return pa, pb, fmt.Errorf("store: lock players: %w", err)
	}
	switch {
	case pa.PlayerID == "":
		return pa, pb, ErrProfileNotFound
	case pb.PlayerID == "":
		return pa, pb, ErrPlayerNotFound
	}
	return pa, pb, nil
}

// blockedEither reports whether either of a and b has blocked the other.
func blockedEither(ctx context.Context, tx pgx.Tx, a, b string) (bool, error) {
	var blocked bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM block
		                 WHERE (blocker = $1 AND blocked = $2) OR (blocker = $2 AND blocked = $1))`,
		a, b).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("store: check block: %w", err)
	}
	return blocked, nil
}

// friendRow reads the friendship between a and b. ok is false when there is none.
func friendRow(ctx context.Context, tx pgx.Tx, a, b string) (state, requestedBy string, ok bool, err error) {
	lo, hi := pair(a, b)
	err = tx.QueryRow(ctx,
		`SELECT state, requested_by::text FROM friendship WHERE player_lo = $1 AND player_hi = $2`,
		lo, hi).Scan(&state, &requestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read friendship: %w", err)
	}
	return state, requestedBy, true, nil
}

// friendCount is how many accepted friends player has.
func friendCount(ctx context.Context, tx pgx.Tx, player string) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM friendship
		  WHERE (player_lo = $1 OR player_hi = $1) AND state = 'accepted'`, player).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count friends: %w", err)
	}
	return n, nil
}

// underLimit is ErrFriendLimit when either player already has max friends.
func underLimit(ctx context.Context, tx pgx.Tx, max int, players ...string) error {
	for _, p := range players {
		n, err := friendCount(ctx, tx, p)
		if err != nil {
			return err
		}
		if n >= max {
			return ErrFriendLimit
		}
	}
	return nil
}

// accept turns the pending friendship between a and b into an accepted one.
func accept(ctx context.Context, tx pgx.Tx, a, b string) error {
	lo, hi := pair(a, b)
	if _, err := tx.Exec(ctx,
		`UPDATE friendship SET state = 'accepted' WHERE player_lo = $1 AND player_hi = $2`, lo, hi); err != nil {
		return fmt.Errorf("store: accept friendship: %w", err)
	}
	return nil
}

func friendNotice(to, typ string, about Friend) Notice {
	payload := map[string]any{"player_id": about.PlayerID}
	if typ != events.TypeFriendRemoved {
		payload["display_name"] = about.DisplayName
		payload["discriminator"] = about.Discriminator
	}
	return Notice{PlayerID: to, Type: typ, Payload: payload}
}

// FindPlayer returns the id of the player with this display name (compared without
// case, as the unique index is) and discriminator, or ErrPlayerNotFound.
func (d *DB) FindPlayer(ctx context.Context, displayName string, discriminator int) (string, error) {
	var id string
	err := d.Pool.QueryRow(ctx,
		`SELECT player_id::text FROM player_profile WHERE lower(display_name) = lower($1) AND discriminator = $2`,
		displayName, discriminator).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPlayerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: find player: %w", err)
	}
	return id, nil
}

// RequestFriend sends a friend request from player to target (SES-C1) and returns target
// with the friendship's state afterwards. A pending request from target to player is
// accepted instead, so two players asking each other end as friends. Asking again while
// a request is pending changes nothing. It is ErrBlocked when either has blocked the
// other, ErrAlreadyFriends when they are friends, and ErrFriendLimit when the request
// or the accept would take either past max.
func (d *DB) RequestFriend(ctx context.Context, player, target string, max int) (Friend, []Notice, error) {
	if player == target {
		return Friend{}, nil, ErrTargetIsYourself
	}
	var (
		them    Friend
		notices []Notice
	)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		me, other, err := lockPlayers(ctx, tx, player, target)
		if err != nil {
			return err
		}
		them = other
		if blocked, err := blockedEither(ctx, tx, player, target); err != nil {
			return err
		} else if blocked {
			return ErrBlocked
		}
		cur, requestedBy, exists, err := friendRow(ctx, tx, player, target)
		if err != nil {
			return err
		}
		switch {
		case exists && cur == FriendAccepted:
			return ErrAlreadyFriends
		case exists && requestedBy == player:
			them.State, them.RequestedBy = FriendPending, player
			return nil
		case exists: // pending, asked by target: this is a yes
			if err := underLimit(ctx, tx, max, player, target); err != nil {
				return err
			}
			if err := accept(ctx, tx, player, target); err != nil {
				return err
			}
			them.State, them.RequestedBy = FriendAccepted, target
			notices = []Notice{friendNotice(target, events.TypeFriendAccepted, me)}
			return nil
		}
		if err := underLimit(ctx, tx, max, player); err != nil {
			return err
		}
		lo, hi := pair(player, target)
		if _, err := tx.Exec(ctx,
			`INSERT INTO friendship (player_lo, player_hi, state, requested_by) VALUES ($1, $2, 'pending', $3)`,
			lo, hi, player); err != nil {
			return fmt.Errorf("store: write friend request: %w", err)
		}
		them.State, them.RequestedBy = FriendPending, player
		notices = []Notice{friendNotice(target, events.TypeFriendRequest, me)}
		return nil
	})
	if err != nil {
		return Friend{}, nil, err
	}
	return them, notices, nil
}

// AcceptFriend accepts the pending request requester sent player (SES-C2). It is
// ErrRequestNotFound when there is none, and ErrFriendLimit when either would pass max.
func (d *DB) AcceptFriend(ctx context.Context, player, requester string, max int) (Friend, []Notice, error) {
	var (
		friend  Friend
		notices []Notice
	)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		me, them, err := lockPlayers(ctx, tx, player, requester)
		if errors.Is(err, ErrPlayerNotFound) {
			return ErrRequestNotFound
		}
		if err != nil {
			return err
		}
		cur, requestedBy, exists, err := friendRow(ctx, tx, player, requester)
		if err != nil {
			return err
		}
		if !exists || cur != FriendPending || requestedBy != requester {
			return ErrRequestNotFound
		}
		if err := underLimit(ctx, tx, max, player, requester); err != nil {
			return err
		}
		if err := accept(ctx, tx, player, requester); err != nil {
			return err
		}
		them.State, them.RequestedBy = FriendAccepted, requester
		friend, notices = them, []Notice{friendNotice(requester, events.TypeFriendAccepted, me)}
		return nil
	})
	return friend, notices, err
}

// DeclineFriend removes the pending request requester sent player, and tells the
// requester with friend.removed. It is ErrRequestNotFound when there is none.
func (d *DB) DeclineFriend(ctx context.Context, player, requester string) ([]Notice, error) {
	lo, hi := pair(player, requester)
	tag, err := d.Pool.Exec(ctx,
		`DELETE FROM friendship
		  WHERE player_lo = $1 AND player_hi = $2 AND state = 'pending' AND requested_by = $3`,
		lo, hi, requester)
	if err != nil {
		return nil, fmt.Errorf("store: decline friend request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrRequestNotFound
	}
	return []Notice{friendNotice(requester, events.TypeFriendRemoved, Friend{PlayerID: player})}, nil
}

// RemoveFriend ends the friendship between player and other, accepted or pending in
// either direction (so it also withdraws a request player sent), and tells other with
// friend.removed. It is ErrNotFriends when there is none.
func (d *DB) RemoveFriend(ctx context.Context, player, other string) ([]Notice, error) {
	lo, hi := pair(player, other)
	tag, err := d.Pool.Exec(ctx,
		`DELETE FROM friendship WHERE player_lo = $1 AND player_hi = $2`, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store: remove friend: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFriends
	}
	return []Notice{friendNotice(other, events.TypeFriendRemoved, Friend{PlayerID: player})}, nil
}

// Block records that player blocks target (SES-C3), and in the same transaction ends
// any friendship or friend request between them and deletes every party invite between
// them, in either direction. Blocking again changes nothing. A friendship that ended
// tells target with friend.removed; nothing tells them they were blocked.
func (d *DB) Block(ctx context.Context, player, target string) ([]Notice, error) {
	if player == target {
		return nil, ErrTargetIsYourself
	}
	var notices []Notice
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, _, err := lockPlayers(ctx, tx, player, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO block (blocker, blocked) VALUES ($1, $2) ON CONFLICT DO NOTHING`, player, target); err != nil {
			return fmt.Errorf("store: write block: %w", err)
		}
		lo, hi := pair(player, target)
		tag, err := tx.Exec(ctx, `DELETE FROM friendship WHERE player_lo = $1 AND player_hi = $2`, lo, hi)
		if err != nil {
			return fmt.Errorf("store: end friendship on block: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM party_invite
			  WHERE (from_player = $1 AND to_player = $2) OR (from_player = $2 AND to_player = $1)`,
			player, target); err != nil {
			return fmt.Errorf("store: delete party invites on block: %w", err)
		}
		if tag.RowsAffected() > 0 {
			notices = []Notice{friendNotice(target, events.TypeFriendRemoved, Friend{PlayerID: player})}
		}
		return nil
	})
	return notices, err
}

// Unblock removes player's block on target. Unblocking someone not blocked changes
// nothing. It does not restore the friendship the block ended.
func (d *DB) Unblock(ctx context.Context, player, target string) error {
	if _, err := d.Pool.Exec(ctx,
		`DELETE FROM block WHERE blocker = $1 AND blocked = $2`, player, target); err != nil {
		return fmt.Errorf("store: unblock: %w", err)
	}
	return nil
}

// ListFriends returns every friendship and request of player, with the other player's
// public name, in one query: accepted friends, requests to player, and requests from
// player, oldest first.
func (d *DB) ListFriends(ctx context.Context, player string) ([]Friend, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT p.player_id::text, p.display_name, p.discriminator, f.state, f.requested_by::text, f.created_at
		   FROM friendship f
		   JOIN player_profile p
		     ON p.player_id = CASE WHEN f.player_lo = $1 THEN f.player_hi ELSE f.player_lo END
		  WHERE f.player_lo = $1 OR f.player_hi = $1
		  ORDER BY f.created_at, p.player_id`, player)
	if err != nil {
		return nil, fmt.Errorf("store: list friends: %w", err)
	}
	defer rows.Close()
	var out []Friend
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.PlayerID, &f.DisplayName, &f.Discriminator, &f.State, &f.RequestedBy, &f.Since); err != nil {
			return nil, fmt.Errorf("store: read friend: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AcceptedFriends returns the ids of player's accepted friends. A friendship is stored
// once, lower id first, so both columns are searched (friendship_hi_idx serves the
// second).
func (d *DB) AcceptedFriends(ctx context.Context, player string) ([]string, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT player_hi::text FROM friendship WHERE player_lo = $1 AND state = 'accepted'
		 UNION ALL
		 SELECT player_lo::text FROM friendship WHERE player_hi = $1 AND state = 'accepted'`, player)
	if err != nil {
		return nil, fmt.Errorf("store: list friends: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read friend: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
