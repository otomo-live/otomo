package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Schema is one row of config_schema: an immutable schema version for a namespace.
type Schema struct {
	Namespace     string
	SchemaVersion int
	Body          json.RawMessage
	CreatedBy     string
	CreatedAt     time.Time
}

// ErrNamespaceNotFound is returned by the schema accessors when the namespace does not
// exist. It is a sentinel so the API layer can map it to a 404 without inspecting the
// driver error.
var ErrNamespaceNotFound = errors.New("namespace not found")

// ErrStaleSchema is returned by ReplaceSchema when the caller's expected schema version
// is no longer the namespace's current one, i.e. someone else replaced it first. It is a
// sentinel so the API layer can map it to a 409 without inspecting the row it read.
var ErrStaleSchema = errors.New("stale schema version")

// StaleSchemaError is the concrete form of ErrStaleSchema. It carries the schema that is
// current so the API's 409 can return it; errors.Is(err, ErrStaleSchema) still reports
// true.
type StaleSchemaError struct {
	Current Schema
}

func (e *StaleSchemaError) Error() string {
	return fmt.Sprintf("schema v%d was saved since the expected version", e.Current.SchemaVersion)
}

func (e *StaleSchemaError) Unwrap() error { return ErrStaleSchema }

// latestSchemaQuery reads the highest schema_version for a namespace. CreateNamespace
// always writes version 1, so an existing namespace always has a row; no rows means
// the namespace itself is unknown.
const latestSchemaQuery = `
SELECT namespace, schema_version, body, created_by, created_at
  FROM config_schema
 WHERE namespace = $1
 ORDER BY schema_version DESC
 LIMIT 1`

// LatestSchema returns the namespace's highest schema version.
//
// A namespace that does not exist returns ErrNamespaceNotFound.
func (d *DB) LatestSchema(ctx context.Context, ns string) (Schema, error) {
	var s Schema
	err := d.Pool.QueryRow(ctx, latestSchemaQuery, ns).Scan(
		&s.Namespace,
		&s.SchemaVersion,
		&s.Body,
		&s.CreatedBy,
		&s.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Schema{}, ErrNamespaceNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("store: latest schema: %w", err)
	}
	return s, nil
}

// ReplaceSchema appends a new schema version for ns and records the act in the audit
// log, all in one transaction.
//
// expected is the optimistic-locking precondition: when it is non-nil the namespace's
// current schema version must equal *expected or the call returns *StaleSchemaError and
// writes nothing. nil means "no check" and is only for internal callers that have just
// observed the version themselves (the seed run); the HTTP handler always passes the
// version the client presented, never nil.
//
// The namespace row is locked FOR UPDATE before the next version is computed, which is
// what serialises concurrent replaces: without it two transactions could both read the
// same MAX and race for the same primary key. An unknown namespace returns
// ErrNamespaceNotFound.
func (d *DB) ReplaceSchema(ctx context.Context, ns string, body []byte, expected *int, actor Entry) (Schema, error) {
	var s Schema

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var locked int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM config_namespace WHERE name = $1 FOR UPDATE`, ns).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNamespaceNotFound
			}
			return fmt.Errorf("store: lock namespace: %w", err)
		}

		var current Schema
		if err := tx.QueryRow(ctx, latestSchemaQuery, ns).Scan(
			&current.Namespace,
			&current.SchemaVersion,
			&current.Body,
			&current.CreatedBy,
			&current.CreatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNamespaceNotFound
			}
			return fmt.Errorf("store: current schema: %w", err)
		}

		if expected != nil && current.SchemaVersion != *expected {
			return &StaleSchemaError{Current: current}
		}
		next := current.SchemaVersion + 1

		if err := tx.QueryRow(ctx,
			`INSERT INTO config_schema (namespace, schema_version, body, created_by)
			 VALUES ($1, $2, $3, $4)
			 RETURNING namespace, schema_version, body, created_by, created_at`,
			ns, next, body, actor.ActorID).Scan(
			&s.Namespace,
			&s.SchemaVersion,
			&s.Body,
			&s.CreatedBy,
			&s.CreatedAt,
		); err != nil {
			return fmt.Errorf("store: insert schema: %w", err)
		}

		audit := actor
		audit.Action = "schema.replace"
		audit.Target = ns
		audit.Details = map[string]any{"schema_version": next}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Schema{}, err
	}
	return s, nil
}
