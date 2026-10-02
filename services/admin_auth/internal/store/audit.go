// Reads for the audit feed served at GET /admin-auth/audit. The row is read back in
// the same shape Config's feed uses, so the Dashboard can merge the two without
// special-casing this service.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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
// filter is not applied. Actor matches actor_id exactly, unlike Config's search which
// also matches actor_name.
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
		    AND ($3::text IS NULL OR actor_id = $3)
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
