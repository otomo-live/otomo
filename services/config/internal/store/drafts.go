package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Draft is one row of config_draft: a namespace's mutable working document, plus the
// optimistic-locking state a save has to present. BaseVersion is nil until a version
// ticket sets it.
type Draft struct {
	Namespace   string
	Body        json.RawMessage
	Revision    int
	BaseVersion *int
	UpdatedBy   string
	UpdatedAt   time.Time
}

// DraftSaved is what SaveDraft returns: the new revision and when it was written.
type DraftSaved struct {
	Revision  int
	UpdatedAt time.Time
}

// ErrStaleRevision is returned by SaveDraft when the caller's revision is no longer
// the draft's current one, i.e. someone else saved first. It is a sentinel so the API
// layer can map it to a 409 without inspecting a row count.
var ErrStaleRevision = errors.New("stale draft revision")

// draftQuery reads the one draft row a namespace can have. config_draft's namespace is
// its primary key, so no ordering is needed.
const draftQuery = `
SELECT namespace, body, revision, base_version, updated_by, updated_at
  FROM config_draft
 WHERE namespace = $1`

// Draft returns the namespace's working draft.
//
// A namespace that does not exist returns ErrNamespaceNotFound.
func (d *DB) Draft(ctx context.Context, ns string) (Draft, error) {
	var draft Draft
	err := d.Pool.QueryRow(ctx, draftQuery, ns).Scan(
		&draft.Namespace,
		&draft.Body,
		&draft.Revision,
		&draft.BaseVersion,
		&draft.UpdatedBy,
		&draft.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNamespaceNotFound
	}
	if err != nil {
		return Draft{}, fmt.Errorf("store: read draft: %w", err)
	}
	return draft, nil
}

// SaveDraft replaces ns's draft body, advancing revision by one, and records the act
// in the audit log — all in one transaction.
//
// The WHERE revision = $2 clause is the optimistic lock: the update matches only when
// the caller's revision is still current, so two concurrent saves at the same revision
// cannot both win. The loser's UPDATE blocks on the row lock, then replans against the
// committed row and matches nothing, and matching nothing is turned into
// ErrStaleRevision — unless the namespace itself is unknown, in which case it is
// ErrNamespaceNotFound.
func (d *DB) SaveDraft(ctx context.Context, ns string, body []byte, revision int, actor Entry) (DraftSaved, error) {
	var saved DraftSaved

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE config_draft
			    SET body = $3, revision = revision + 1, updated_by = $4, updated_at = now()
			  WHERE namespace = $1 AND revision = $2
			  RETURNING revision, updated_at`,
			ns, revision, body, actor.ActorID).Scan(&saved.Revision, &saved.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Zero rows has two causes. The namespace check runs after the failed
			// update on purpose: it can only be reached by the loser, and by then the
			// winner has committed, so the answer is the committed one.
			var exists int
			if err := tx.QueryRow(ctx,
				`SELECT 1 FROM config_namespace WHERE name = $1`, ns).Scan(&exists); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNamespaceNotFound
				}
				return fmt.Errorf("store: check namespace: %w", err)
			}
			return ErrStaleRevision
		}
		if err != nil {
			return fmt.Errorf("store: save draft: %w", err)
		}

		audit := actor
		audit.Action = "draft.save"
		audit.Target = ns
		audit.Details = map[string]any{"revision": saved.Revision}
		return WriteAudit(ctx, tx, audit)
	})
	if err != nil {
		return DraftSaved{}, err
	}
	return saved, nil
}
