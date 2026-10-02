// Staff routes (SE-7; design/04-session-minimal.md SES-A5): player lookup,
// force-disband, and the audit feed the Dashboard merges.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// ErrPartyNotFound is a staff call naming a party that does not exist.
var ErrPartyNotFound = errors.New("store: no such party")

// ActionDisbandForced is the audit action of a staff force-disband (SES-A5).
const ActionDisbandForced = "party.disband_forced"

// maxLookup caps GET /players: a name is shared by at most 9,999 discriminators, and a
// staff screen has no use for more than a page of them.
const maxLookup = 50

// LookupPlayers returns the profiles whose display name is name (compared without case),
// narrowed to discriminator when it is not nil, ordered by discriminator.
func (d *DB) LookupPlayers(ctx context.Context, name string, discriminator *int) ([]Profile, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT player_id::text, display_name, discriminator FROM player_profile
		  WHERE lower(display_name) = lower($1) AND ($2::int IS NULL OR discriminator = $2)
		  ORDER BY discriminator LIMIT $3`, name, discriminator, maxLookup)
	if err != nil {
		return nil, fmt.Errorf("store: look up players: %w", err)
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var (
			p  Profile
			id string
		)
		if err := rows.Scan(&id, &p.DisplayName, &p.Discriminator); err != nil {
			return nil, fmt.Errorf("store: read player: %w", err)
		}
		if p.PlayerID, err = uuid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: read player id: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ForceDisband deletes a party in any state, as staff (doc 14 §2 note 2: a match that is
// running is not cancelled, and the Allocator's later callback finds no party). The
// audit row is written in the same transaction as the delete (SES-A5). Every member gets
// party.disbanded. It is ErrPartyNotFound when the party does not exist.
func (d *DB) ForceDisband(ctx context.Context, partyID string, actor Entry) (*Party, []Notice, error) {
	var (
		party   *Party
		notices []Notice
	)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		p, err := loadParty(ctx, tx, partyID, true)
		if errors.Is(err, ErrNotInParty) {
			return ErrPartyNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM party WHERE party_id = $1`, partyID); err != nil {
			return fmt.Errorf("store: force disband: %w", err)
		}
		details := map[string]any{
			"state":    p.State,
			"leader":   p.LeaderID,
			"members":  p.memberIDs(),
			"revision": p.Revision,
		}
		if p.AllocationID != "" {
			details["allocation_id"] = p.AllocationID
		}
		for k, v := range actor.Details {
			details[k] = v
		}
		if err := WriteAudit(ctx, tx, Entry{
			ActorID: actor.ActorID, ActorName: actor.ActorName,
			Action: ActionDisbandForced, Target: partyID, Details: details,
		}); err != nil {
			return err
		}
		for _, id := range p.memberIDs() {
			notices = append(notices, Notice{PlayerID: id, Type: events.TypePartyDisbanded,
				Payload: map[string]any{"party_id": p.ID, "revision": p.Revision + 1}})
		}
		party = p
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return party, notices, nil
}

// AuditRecord is one audit_log row read back for the audit feed. Details is the raw
// jsonb body, so the feed can embed it as a JSON object.
type AuditRecord struct {
	ID        int64
	At        time.Time
	ActorID   string
	ActorName string
	Action    string
	Target    string
	Details   json.RawMessage
}

// AuditQuery narrows the audit feed; a nil field is not applied. Actor matches actor_id
// or actor_name, as Config's does, because the Dashboard searches by either.
type AuditQuery struct {
	Before *int64
	Action *string
	Actor  *string
	From   *time.Time
	To     *time.Time
}

// ListAudit returns audit entries newest first, at most limit, and the next page's
// cursor id when more exist. It is Config's query on Session's table, so the Dashboard
// pages both feeds the same way.
func (d *DB) ListAudit(ctx context.Context, q AuditQuery, limit int) ([]AuditRecord, *int64, error) {
	// One extra row tells a full page from the last one; it is not returned.
	rows, err := d.Pool.Query(ctx,
		`SELECT id, at, actor_id, actor_name, action, target, details
		   FROM audit_log
		  WHERE ($1::bigint IS NULL OR id < $1)
		    AND ($2::text IS NULL OR action = $2)
		    AND ($3::text IS NULL OR actor_id = $3 OR actor_name = $3)
		    AND ($4::timestamptz IS NULL OR at >= $4)
		    AND ($5::timestamptz IS NULL OR at <= $5)
		  ORDER BY id DESC
		  LIMIT $6`,
		q.Before, q.Action, q.Actor, q.From, q.To, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()

	entries := make([]AuditRecord, 0, limit)
	for rows.Next() {
		var (
			e       AuditRecord
			details []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &e.ActorID, &e.ActorName, &e.Action, &e.Target, &details); err != nil {
			return nil, nil, fmt.Errorf("store: read audit entry: %w", err)
		}
		e.Details = details
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list audit: %w", err)
	}
	var next *int64
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[len(entries)-1].ID
		next = &last
	}
	return entries, next, nil
}
