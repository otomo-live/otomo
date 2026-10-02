package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Entry is one audit_log row: who did what, to what, and with what detail.
//
// The one action written today is SES-A5's `party.disband_forced`, from the staff
// force-disband route; the Phase B to D handlers add their own as they land. The table
// and this writer are the same shape as Config's (design/02-config.md §3), deliberately:
// Dashboard reads both services' trails, and one table per service means it unions them
// without translating columns.
type Entry struct {
	ActorID   string         // the staff `sub` from the verified token
	ActorName string         // denormalized; PHP's staff table is the source
	Action    string         // e.g. "release.publish"
	Target    string         // the namespace, pack or channel the action applied to
	Details   map[string]any // may be nil; stored as '{}'
}

// WriteAudit appends e to audit_log using tx.
//
// It takes a pgx.Tx rather than the pool on purpose (SES-A5). The audit entry has to
// land in the same transaction as the change it describes, so that either both are
// committed or neither is; a caller that passed the pool could produce an audit trail
// naming a change that was rolled back. That is worse than a missing entry, because a
// wrong record is indistinguishable from a right one to whoever reads it later — and
// §7's definition of done has a force-disband visible in the Dashboard's trail.
//
// ActorName is required even though it is derivable, because the table denormalizes it
// deliberately: the audit log is read long after a staff account may have been renamed
// or deleted, and a row that says "someone did this" is not an audit trail.
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
