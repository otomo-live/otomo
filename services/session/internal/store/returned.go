// Returning a lobby from its match (LB-4; design/14-launch-handoff.md §2, §4.4,
// §6, §7).
//
// A match ends in one of two ways as far as Session can tell: the Allocator's callback,
// or the repair poll finding the allocation no longer live. Both finish with
// ReturnFromGame, whose update is conditional on the party still being in_game on that
// allocation, so the second of the two changes nothing and returns no notices.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// ReturnFromGame puts a party whose match on allocationID has ended back into forming
// (doc 14 §2): every ready flag cleared, the match's allocation, address and port
// cleared, and the revision bumped. Every member gets party.updated and
// party.returned{party_id, revision, reason}. done is false, with nothing changed, when
// the party is gone, is not in_game, or is in_game on another allocation; the callback
// answers 204 all the same (doc 14 §4.4).
func (d *DB) ReturnFromGame(ctx context.Context, partyID, allocationID, reason string) (p *Party, notices []Notice, done bool, err error) {
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		// The update takes the party row lock before any member row, as every party
		// change does.
		tag, err := tx.Exec(ctx,
			`UPDATE party SET state = 'forming', state_changed_at = now(),
			        allocation_id = NULL, match_address = NULL, match_port = NULL
			  WHERE party_id = $1 AND state = 'in_game' AND allocation_id = $2`, partyID, allocationID)
		if err != nil {
			return fmt.Errorf("store: return from game: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE party_member SET ready = false WHERE party_id = $1`, partyID); err != nil {
			return fmt.Errorf("store: clear ready flags: %w", err)
		}
		cur := &Party{ID: partyID}
		if err := bumpRevision(ctx, tx, cur); err != nil {
			return err
		}
		if err := reload(ctx, tx, cur); err != nil {
			return err
		}
		notices = updated(cur)
		for _, id := range cur.memberIDs() {
			notices = append(notices, Notice{PlayerID: id, Type: events.TypePartyReturned,
				Payload: map[string]any{"party_id": cur.ID, "revision": cur.Revision, "reason": reason}})
		}
		p, done = cur, true
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return p, notices, done, nil
}

// InGame is one party found by InGameLongerThan.
type InGame struct {
	PartyID      string
	AllocationID string
}

// InGameLongerThan lists parties that have been in_game for longer than olderThan, for
// the repair poll that catches a missed callback (doc 14 §6). The oldest come first.
func (d *DB) InGameLongerThan(ctx context.Context, olderThan time.Duration) ([]InGame, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT party_id::text, allocation_id::text FROM party
		  WHERE state = 'in_game' AND allocation_id IS NOT NULL
		    AND state_changed_at < now() - make_interval(secs => $1)
		  ORDER BY state_changed_at, party_id`, olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: find parties in a match: %w", err)
	}
	defer rows.Close()

	var out []InGame
	for rows.Next() {
		var g InGame
		if err := rows.Scan(&g.PartyID, &g.AllocationID); err != nil {
			return nil, fmt.Errorf("store: read party in a match: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
