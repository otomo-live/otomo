package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Version is one immutable snapshot of a namespace's config document. Body is set only
// by GetVersion: CreateVersion already holds the canonical bytes it just wrote, and the
// history list does not read the body at all.
type Version struct {
	Namespace     string
	Version       int
	SchemaVersion int
	SHA256        string
	Message       string
	CreatedBy     string
	Size          int
	CreatedAt     time.Time

	// Body is the stored jsonb document, present on GetVersion and nil elsewhere.
	Body json.RawMessage
}

// VersionSummary is one row of the version history list. It omits the body on purpose:
// the list can return up to 200 rows, and neither the document nor its canonical size
// belongs in a paging response.
type VersionSummary struct {
	Version       int
	SchemaVersion int
	SHA256        string
	Message       string
	CreatedBy     string
	CreatedAt     time.Time
}

// ErrVersionNotFound is returned by GetVersion when the namespace or the version does
// not exist. Both are a 404 at the API layer, so they share a sentinel.
var ErrVersionNotFound = errors.New("version not found")

// ErrNoChanges is returned by CreateVersion when the draft's canonical bytes are
// already the latest version's. It is a sentinel so the API layer can map it to a 409
// without inspecting a hash.
var ErrNoChanges = errors.New("draft has no changes")

// NoChangesError is the concrete form of ErrNoChanges. It carries the latest version's
// number so the API's message can name what the draft already matches; errors.Is(err,
// ErrNoChanges) still reports true.
type NoChangesError struct {
	Version int
}

func (e *NoChangesError) Error() string {
	return fmt.Sprintf("draft is identical to version %d", e.Version)
}

func (e *NoChangesError) Unwrap() error { return ErrNoChanges }

// CreateVersion turns ns's current draft into a new immutable version, in one
// transaction.
//
// The draft row is locked FOR UPDATE before anything else, which is what serialises
// version creation per namespace: a caller that presents a revision the draft has moved
// past gets ErrStaleRevision, and only one caller can reach the INSERT for a given
// revision.
//
// prepare is called with the draft body, the latest schema version and its body. The
// API layer's prepare validates the draft against that schema, canonicalises it and
// writes the canonical bytes to the blob store; it runs before the INSERT on purpose,
// because an orphan blob is harmless while a version row pointing at a missing blob is
// not. An error from prepare aborts the transaction and is returned unchanged.
func (d *DB) CreateVersion(ctx context.Context, ns string, revision int, message string, actor Entry,
	prepare func(draftBody []byte, schemaVersion int, schemaBody []byte) (canonical []byte, sha string, err error)) (Version, error) {
	var v Version

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var (
			draftBody []byte
			current   int
		)
		err := tx.QueryRow(ctx,
			`SELECT body, revision FROM config_draft WHERE namespace = $1 FOR UPDATE`,
			ns).Scan(&draftBody, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNamespaceNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock draft: %w", err)
		}
		if current != revision {
			return ErrStaleRevision
		}

		var (
			schemaVersion int
			schemaBody    []byte
			schemaBy      string
			schemaAt      time.Time
		)
		if err := tx.QueryRow(ctx, latestSchemaQuery, ns).Scan(
			&v.Namespace, &schemaVersion, &schemaBody, &schemaBy, &schemaAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNamespaceNotFound
			}
			return fmt.Errorf("store: latest schema: %w", err)
		}

		canonical, sha, err := prepare(draftBody, schemaVersion, schemaBody)
		if err != nil {
			return err
		}

		var (
			latestVersion int
			latestSHA     string
		)
		err = tx.QueryRow(ctx,
			`SELECT version, sha256 FROM config_version
			  WHERE namespace = $1
			  ORDER BY version DESC
			  LIMIT 1`, ns).Scan(&latestVersion, &latestSHA)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// First version: nothing to compare against.
		case err != nil:
			return fmt.Errorf("store: latest version: %w", err)
		case latestSHA == sha:
			return &NoChangesError{Version: latestVersion}
		}

		var next int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM config_version WHERE namespace = $1`, ns).Scan(&next); err != nil {
			return fmt.Errorf("store: next version: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`INSERT INTO config_version (namespace, version, schema_version, body, sha256, message, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING created_at`,
			ns, next, schemaVersion, canonical, sha, message, actor.ActorID).Scan(&v.CreatedAt); err != nil {
			return fmt.Errorf("store: insert version: %w", err)
		}

		// The revision and body are deliberately left alone: creating a version does
		// not edit the draft, it only records which version the draft is based on.
		if _, err := tx.Exec(ctx,
			`UPDATE config_draft SET base_version = $2 WHERE namespace = $1`, ns, next); err != nil {
			return fmt.Errorf("store: update draft base version: %w", err)
		}

		audit := actor
		audit.Action = "version.create"
		audit.Target = ns
		audit.Details = map[string]any{
			"version":        next,
			"schema_version": schemaVersion,
			"sha256":         sha,
		}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}

		v.Version = next
		v.SchemaVersion = schemaVersion
		v.SHA256 = sha
		v.Message = message
		v.CreatedBy = actor.ActorID
		v.Size = len(canonical)
		return nil
	})
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

// ListVersions returns ns's versions newest first, at most limit of them, plus the next
// page's cursor when there are more rows. before, when non-nil, restricts the page to
// versions below it.
//
// An unknown namespace returns ErrNamespaceNotFound, which is what lets the list route
// tell "nothing published yet" apart from "no such namespace".
func (d *DB) ListVersions(ctx context.Context, ns string, before *int, limit int) ([]VersionSummary, *int, error) {
	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM config_namespace WHERE name = $1)`, ns).Scan(&exists); err != nil {
		return nil, nil, fmt.Errorf("store: check namespace: %w", err)
	}
	if !exists {
		return nil, nil, ErrNamespaceNotFound
	}

	// One extra row is read to tell a full page from the last page; it is not returned.
	rows, err := d.Pool.Query(ctx,
		`SELECT version, schema_version, sha256, message, created_by, created_at
		   FROM config_version
		  WHERE namespace = $1 AND ($2::int IS NULL OR version < $2)
		  ORDER BY version DESC
		  LIMIT $3`, ns, before, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list versions: %w", err)
	}
	defer rows.Close()

	versions := make([]VersionSummary, 0, limit)
	for rows.Next() {
		var v VersionSummary
		if err := rows.Scan(&v.Version, &v.SchemaVersion, &v.SHA256, &v.Message, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: scan version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list versions: %w", err)
	}

	var next *int
	if len(versions) > limit {
		versions = versions[:limit]
		last := versions[len(versions)-1].Version
		next = &last
	}
	return versions, next, nil
}

// GetVersion returns one version, body included, so the caller can render the document
// and compute the canonical size the list endpoint does not carry.
//
// An unknown namespace and an unknown version both return ErrVersionNotFound: the API
// answers both with the same 404.
func (d *DB) GetVersion(ctx context.Context, ns string, version int) (Version, error) {
	var v Version
	err := d.Pool.QueryRow(ctx,
		`SELECT namespace, version, schema_version, sha256, message, created_by, created_at, body
		   FROM config_version
		  WHERE namespace = $1 AND version = $2`, ns, version).Scan(
		&v.Namespace, &v.Version, &v.SchemaVersion, &v.SHA256,
		&v.Message, &v.CreatedBy, &v.CreatedAt, &v.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrVersionNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("store: get version: %w", err)
	}
	return v, nil
}
