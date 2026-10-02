// Launching a lobby (LB-3; design/14-launch-handoff.md §3, §6).
//
// A launch is three steps with the Allocator call outside any transaction: StartLaunch
// commits launching, the caller asks the Allocator, and FinishLaunch or FailLaunch
// commits the outcome. Both finishers lock the party and act only while it is still
// launching, so a duplicate finisher (the stuck-launch sweep racing the original call)
// changes nothing and returns no notices.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// InvalidSettingsError is a launch refused because the lobby's settings no longer fit
// the rules in force.
type InvalidSettingsError struct{ Reason string }

func (e *InvalidSettingsError) Error() string { return "store: invalid lobby settings: " + e.Reason }

// StartLaunch moves the leader's lobby from forming to launching (doc 14 §2): leader
// only, at the current revision, with every member ready and settings that check
// accepts. It returns started=true with the members to allocate for when it made the
// change. A lobby already launching is not an error: started is false and nothing
// changes, so a second press starts no second allocation. A lobby in_game is
// ErrPartyLocked.
func (d *DB) StartLaunch(ctx context.Context, leader string, revision int,
	check func(settings map[string]string) error) (p *Party, notices []Notice, members []string, started bool, err error) {

	p, notices, err = d.mutate(ctx, leader, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if p.LeaderID != leader {
			return nil, ErrNotLeader
		}
		switch p.State {
		case StateInGame:
			return nil, ErrPartyLocked
		case StateLaunching:
			return nil, nil
		}
		if revision != p.Revision {
			return nil, &RevisionMismatchError{Current: p.Revision}
		}
		for _, m := range p.Members {
			if !m.Ready {
				return nil, ErrNotReady
			}
		}
		if check != nil {
			if err := check(p.Settings); err != nil {
				return nil, &InvalidSettingsError{Reason: err.Error()}
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party SET state = 'launching', state_changed_at = now() WHERE party_id = $1`, p.ID); err != nil {
			return nil, fmt.Errorf("store: start launch: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		started = true
		return updated(p), nil
	})
	if err != nil {
		return nil, nil, nil, false, err
	}
	return p, notices, p.memberIDs(), started, nil
}

// FinishLaunch records a granted allocation: launching becomes in_game with the match's
// allocation and public address. It returns the party's current members, who each get
// a party.launching with their own ticket from the caller, and party.updated notices.
// done is false when the party was not launching any more (another finisher won, or it
// was disbanded); then nothing changed.
func (d *DB) FinishLaunch(ctx context.Context, partyID, allocationID, address string, port int) (p *Party, notices []Notice, done bool, err error) {
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		cur, err := loadParty(ctx, tx, partyID, true)
		if err != nil {
			return err
		}
		if cur.State != StateLaunching {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party SET state = 'in_game', state_changed_at = now(),
			        allocation_id = $2, match_address = $3, match_port = $4
			  WHERE party_id = $1`, partyID, allocationID, address, port); err != nil {
			return fmt.Errorf("store: finish launch: %w", err)
		}
		if err := bumpRevision(ctx, tx, cur); err != nil {
			return err
		}
		if err := reload(ctx, tx, cur); err != nil {
			return err
		}
		p, notices, done = cur, updated(cur), true
		return nil
	})
	if errors.Is(err, ErrNotInParty) {
		return nil, nil, false, nil
	}
	return p, notices, done, err
}

// FailLaunch puts a launching party back to forming with reason (no_capacity or
// allocator_unavailable, doc 14 §7). Ready flags are kept so the leader can retry.
// Every member gets party.launch_failed and party.updated. done is false when the party
// was not launching any more.
func (d *DB) FailLaunch(ctx context.Context, partyID, reason string) (p *Party, notices []Notice, done bool, err error) {
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		cur, err := loadParty(ctx, tx, partyID, true)
		if err != nil {
			return err
		}
		if cur.State != StateLaunching {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party SET state = 'forming', state_changed_at = now() WHERE party_id = $1`, partyID); err != nil {
			return fmt.Errorf("store: fail launch: %w", err)
		}
		if err := bumpRevision(ctx, tx, cur); err != nil {
			return err
		}
		if err := reload(ctx, tx, cur); err != nil {
			return err
		}
		notices = updated(cur)
		for _, id := range cur.memberIDs() {
			notices = append(notices, Notice{PlayerID: id, Type: events.TypePartyLaunchFailed,
				Payload: map[string]any{"party_id": cur.ID, "revision": cur.Revision, "reason": reason}})
		}
		p, done = cur, true
		return nil
	})
	if errors.Is(err, ErrNotInParty) {
		return nil, nil, false, nil
	}
	return p, notices, done, err
}

// Launching is one party found by StuckLaunching.
type Launching struct {
	PartyID string
	Members []string
}

// StuckLaunching lists parties that have been launching for longer than olderThan: their
// launch call was lost, most likely with a Session restart (doc 14 §6). The sweep asks
// the Allocator again for each; the call is idempotent per party.
func (d *DB) StuckLaunching(ctx context.Context, olderThan time.Duration) ([]Launching, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT p.party_id::text, m.player_id::text
		   FROM party p JOIN party_member m USING (party_id)
		  WHERE p.state = 'launching' AND p.state_changed_at < now() - make_interval(secs => $1)
		  ORDER BY p.party_id, m.joined_at, m.player_id`, olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: find stuck launches: %w", err)
	}
	defer rows.Close()

	var out []Launching
	for rows.Next() {
		var partyID, player string
		if err := rows.Scan(&partyID, &player); err != nil {
			return nil, fmt.Errorf("store: read stuck launch: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].PartyID != partyID {
			out = append(out, Launching{PartyID: partyID})
		}
		out[len(out)-1].Members = append(out[len(out)-1].Members, player)
	}
	return out, rows.Err()
}
