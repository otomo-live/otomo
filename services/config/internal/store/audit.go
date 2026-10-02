package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Entry is one audit_log row: who did what, to what, and with what detail. The
// actions written today's handlers use are the ones design/02-config.md §3 lists —
// draft.save, version.create, release.publish, release.rollback, pack.upload — and
// each Phase B task adds its own.
type Entry struct {
	ActorID   string         // the staff `sub` from the verified token
	ActorName string         // denormalized; PHP's staff table is the source
	Action    string         // e.g. "release.publish"
	Target    string         // the namespace, pack or channel the action applied to
	Details   map[string]any // may be nil; stored as '{}'
}

// WriteAudit appends e to audit_log using tx.
//
// It takes a pgx.Tx rather than the pool on purpose (CFG-A4). The audit entry has to
// land in the same transaction as the change it describes, so that either both are
// committed or neither is; a caller that passed the pool could produce an audit trail
// naming a change that was rolled back. That is worse than a missing entry, because a
// wrong record is indistinguishable from a right one to whoever reads it later — and
// §7's definition of done has every change visible in the Dashboard's trail.
//
// ActorName is required even though it is derivable, because §3 denormalizes it
// deliberately: the audit log is read long after a staff account may have been
// renamed or deleted, and a row that says "someone did this" is not an audit trail.
func WriteAudit(ctx context.Context, tx pgx.Tx, e Entry) error {
	switch {
	case e.ActorID == "":
		return errors.New("store: audit entry has no actor_id")
	case e.ActorName == "":
		return errors.New("store: audit entry has no actor_name")
	case e.Action == "":
		return errors.New("store: audit entry has no action")
	case e.Target == "":
		return errors.New("store: audit entry has no target")
	}

	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	body, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("store: encode audit details: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_log (actor_id, actor_name, action, target, details)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.ActorID, e.ActorName, e.Action, e.Target, body)
	if err != nil {
		return fmt.Errorf("store: write audit entry: %w", err)
	}
	return nil
}

// AuditRecord is one audit_log row read back for the audit feed. Details is the raw
// jsonb body so the API can embed it as a JSON object rather than a string.
type AuditRecord struct {
	ID        int64
	At        time.Time
	ActorID   string
	ActorName string
	Action    string
	Target    string
	Details   json.RawMessage
}

// AuditQuery narrows the audit feed. Every field is optional; a nil pointer means the
// filter is not applied. Actor matches actor_id OR actor_name, because the Dashboard
// lets staff search by either.
type AuditQuery struct {
	Before *int64
	Action *string
	Actor  *string
	From   *time.Time
	To     *time.Time
}

// ListAudit returns audit entries newest first, at most limit of them, plus the next
// page's cursor id when more rows exist. before, when non-nil, restricts the page to
// rows below it.
func (d *DB) ListAudit(ctx context.Context, q AuditQuery, limit int) ([]AuditRecord, *int64, error) {
	// One extra row tells a full page from the last page; it is not returned.
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
		if err := rows.Scan(&e.ID, &e.At, &e.ActorID, &e.ActorName,
			&e.Action, &e.Target, &details); err != nil {
			return nil, nil, fmt.Errorf("store: scan audit: %w", err)
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
