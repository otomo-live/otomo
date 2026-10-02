package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Namespace is one row of the admin namespace list, with the namespace's latest
// published version and its working draft folded in. It is the shape both
// ListNamespaces and CreateNamespace return, so a client can render a freshly created
// namespace from the same fields it later reads back.
type Namespace struct {
	Name        string
	Audience    string
	Description string // "" when the column is NULL
	CreatedAt   time.Time

	// LatestVersion is the highest config_version.version for the namespace, or nil
	// when nothing has been published yet.
	LatestVersion *int

	Draft DraftStatus
}

// DraftStatus is the working draft's optimistic-locking state, plus whether it differs
// from what is published.
type DraftStatus struct {
	Revision  int
	UpdatedAt time.Time

	// HasUnpublishedChanges is true when the draft has diverged from the latest
	// published version. Before any version exists that means "more than the initial
	// revision"; afterwards it is a structural jsonb comparison against the latest
	// version's body.
	HasUnpublishedChanges bool
}

// ErrNamespaceExists is returned by CreateNamespace when the name is already taken.
// It is a sentinel rather than a wrapped pgx error so the API layer can map it to a
// 409 without inspecting driver error codes itself.
var ErrNamespaceExists = errors.New("namespace already exists")

// listNamespacesQuery reads the whole admin list in one round trip. The LATERAL join
// picks each namespace's latest version and its body, so the unpublished-changes test
// compares the draft against exactly that version.
//
// The JOIN on config_draft is inner because a namespace without a draft is not a state
// any write path can produce; CreateNamespace writes all three rows in one
// transaction.
const listNamespacesQuery = `
SELECT n.name,
       n.audience,
       COALESCE(n.description, ''),
       n.created_at,
       v.version,
       d.revision,
       d.updated_at,
       CASE
           WHEN v.version IS NULL THEN d.revision > 1
           ELSE d.body IS DISTINCT FROM v.body
       END
  FROM config_namespace n
  JOIN config_draft d ON d.namespace = n.name
  LEFT JOIN LATERAL (
      SELECT version, body
        FROM config_version
       WHERE namespace = n.name
       ORDER BY version DESC
       LIMIT 1
  ) v ON true
 ORDER BY n.name`

// ListNamespaces returns every namespace, ordered by name, each with its latest
// version and draft state.
func (d *DB) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	rows, err := d.Pool.Query(ctx, listNamespacesQuery)
	if err != nil {
		return nil, fmt.Errorf("store: list namespaces: %w", err)
	}
	defer rows.Close()

	namespaces := make([]Namespace, 0)
	for rows.Next() {
		var ns Namespace
		if err := rows.Scan(
			&ns.Name,
			&ns.Audience,
			&ns.Description,
			&ns.CreatedAt,
			&ns.LatestVersion,
			&ns.Draft.Revision,
			&ns.Draft.UpdatedAt,
			&ns.Draft.HasUnpublishedChanges,
		); err != nil {
			return nil, fmt.Errorf("store: scan namespace: %w", err)
		}
		namespaces = append(namespaces, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list namespaces: %w", err)
	}
	return namespaces, nil
}

// CreateNamespace creates a namespace together with its initial schema version and
// working draft, and records the act in the audit log — all in one transaction, so a
// half-created namespace or an audit row without its change cannot survive.
//
// A name conflict returns ErrNamespaceExists. The returned Namespace has the same
// shape ListNamespaces produces for the row: no published version, revision 1, and no
// unpublished changes.
func (d *DB) CreateNamespace(ctx context.Context, name, audience, description string, actor Entry) (Namespace, error) {
	ns := Namespace{Name: name, Audience: audience, Description: description}

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		// The column is nullable and "" and NULL are the same fact; store NULL so the
		// database never has to distinguish an absent description from an empty one.
		var desc any
		if description != "" {
			desc = description
		}

		if err := tx.QueryRow(ctx,
			`INSERT INTO config_namespace (name, audience, description)
			 VALUES ($1, $2, $3)
			 RETURNING created_at`,
			name, audience, desc).Scan(&ns.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return ErrNamespaceExists
			}
			return fmt.Errorf("store: insert namespace: %w", err)
		}

		// Start the schema at version 1 with an empty object. A namespace without a
		// schema could not be validated against, so the initial one is part of what
		// "create" means.
		if _, err := tx.Exec(ctx,
			`INSERT INTO config_schema (namespace, schema_version, body, created_by)
			 VALUES ($1, 1, '{}'::jsonb, $2)`,
			name, actor.ActorID); err != nil {
			return fmt.Errorf("store: insert initial schema: %w", err)
		}

		// The working draft starts empty, unbased and at revision 1. revision 1 is the
		// value a first save must present, and the value ListNamespaces treats as
		// "nothing unpublished yet".
		if err := tx.QueryRow(ctx,
			`INSERT INTO config_draft (namespace, body, base_version, revision, updated_by)
			 VALUES ($1, '{}'::jsonb, NULL, 1, $2)
			 RETURNING revision, updated_at`,
			name, actor.ActorID).Scan(&ns.Draft.Revision, &ns.Draft.UpdatedAt); err != nil {
			return fmt.Errorf("store: insert initial draft: %w", err)
		}

		audit := actor
		audit.Action = "namespace.create"
		audit.Target = name
		audit.Details = map[string]any{"audience": audience}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Namespace{}, err
	}

	ns.LatestVersion = nil
	ns.Draft.HasUnpublishedChanges = false
	return ns, nil
}

// isUniqueViolation reports whether err is Postgres' unique_violation, which for the
// namespace insert can only be the name primary key.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
