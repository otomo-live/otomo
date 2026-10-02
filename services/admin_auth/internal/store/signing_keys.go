// Queries for the signing_key table. Only public halves live here — the private key
// stays in the operator's PEM file and never reaches Postgres.

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SigningKey is one row of signing_key: the key ID published in the JWT header, and
// the Ed25519 public key that verifies tokens carrying it.
type SigningKey struct {
	Kid       string
	PublicKey []byte
}

// InsertSigningKey inserts a new active signing key. A duplicate kid is rejected by
// the table's primary key, which is what stops `genkey -kid X` from being run twice
// against one database.
func (d *DB) InsertSigningKey(ctx context.Context, kid string, publicKey []byte) error {
	_, err := d.Pool.Exec(ctx,
		`INSERT INTO signing_key (kid, public_key, active) VALUES ($1, $2, true)`,
		kid, publicKey)
	if err != nil {
		return fmt.Errorf("insert signing key %q: %w", kid, err)
	}
	return nil
}

// ListActiveSigningKeys returns every active key, ordered by kid.
//
// This is the query the JWKS is built from, so whatever it returns is what the
// service publishes for others to verify against. Nothing marks a key inactive yet,
// so today it returns every key ever generated; a rotation story has to add that
// before a key can be retired.
func (d *DB) ListActiveSigningKeys(ctx context.Context) ([]SigningKey, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT kid, public_key FROM signing_key WHERE active = true ORDER BY kid`)
	if err != nil {
		return nil, fmt.Errorf("list active signing keys: %w", err)
	}
	defer rows.Close()

	var out []SigningKey
	for rows.Next() {
		var k SigningKey
		if err := rows.Scan(&k.Kid, &k.PublicKey); err != nil {
			return nil, fmt.Errorf("scan signing key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active signing keys: %w", err)
	}
	return out, nil
}

// FindActiveByPublicKey returns the kid registered for publicKey, or an error
// wrapping ErrNotFound when no active row matches.
//
// serve uses it to confirm the key file it was given is one this service actually
// publishes: a valid key file with no row would sign tokens that no verifier could
// resolve a kid for, which fails at every consumer rather than here.
func (d *DB) FindActiveByPublicKey(ctx context.Context, publicKey []byte) (string, error) {
	var kid string
	err := d.Pool.QueryRow(ctx,
		`SELECT kid FROM signing_key WHERE public_key = $1 AND active = true`,
		publicKey).Scan(&kid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("signing key not registered: %w", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("find active signing key: %w", err)
	}
	return kid, nil
}
