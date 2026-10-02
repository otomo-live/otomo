package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otomo-live/otomo/services/config/internal/blob"
)

// Pack is one row of content_pack: an uploaded binary identified by the SHA-256 of its
// bytes. sha256 is UNIQUE, so the same content uploaded twice is one row — the
// original name and uploader are what a re-upload returns.
type Pack struct {
	PackID     string
	Name       string
	SHA256     string
	Size       int64
	UploadedBy string
	UploadedAt time.Time
}

// packColumns is the column list the two pack queries share, with pack_id cast to text
// so it scans into a string without the caller having to decode a uuid.
const packColumns = `pack_id::text, name, sha256, size_bytes, uploaded_by, uploaded_at`

// CreatePack records an uploaded pack in e's name and returns the row, plus whether
// this call created it. Content addressing makes the call idempotent: a second upload
// of the same bytes hits the sha256 unique index, loads the original row and reports
// created=false, leaving the original name, uploader and timestamp untouched.
//
// The insert and the pack.upload audit entry share a transaction, so the audit trail
// can never name an upload that was not recorded (CFG-A4). The audit is written only
// when this call actually created the row: a cache hit is not an upload event.
func (d *DB) CreatePack(ctx context.Context, name string, ref blob.Ref, actor Entry) (Pack, bool, error) {
	var (
		p       Pack
		created bool
	)

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO content_pack (pack_id, name, sha256, size_bytes, uploaded_by)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4)
			 ON CONFLICT (sha256) DO NOTHING
			 RETURNING `+packColumns,
			name, ref.SHA256, ref.Size, actor.ActorID).Scan(
			&p.PackID, &p.Name, &p.SHA256, &p.Size, &p.UploadedBy, &p.UploadedAt)
		switch {
		case err == nil:
			created = true

		case errors.Is(err, pgx.ErrNoRows):
			// The bytes already exist under some earlier pack. Return that row rather
			// than the name the caller presented, which is the service's contract:
			// one piece of content has one identity.
			if err := tx.QueryRow(ctx,
				`SELECT `+packColumns+` FROM content_pack WHERE sha256 = $1`,
				ref.SHA256).Scan(
				&p.PackID, &p.Name, &p.SHA256, &p.Size, &p.UploadedBy, &p.UploadedAt); err != nil {
				return fmt.Errorf("store: load existing pack: %w", err)
			}
			return nil

		default:
			return fmt.Errorf("store: insert pack: %w", err)
		}

		audit := actor
		audit.Action = "pack.upload"
		audit.Target = p.Name
		audit.Details = map[string]any{
			"name":   p.Name,
			"sha256": p.SHA256,
			"size":   p.Size,
		}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Pack{}, false, err
	}
	return p, created, nil
}

// ListPacks returns every uploaded pack newest first. The list is always a non-nil
// slice, so the API can render an empty array rather than null.
func (d *DB) ListPacks(ctx context.Context) ([]Pack, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT `+packColumns+` FROM content_pack
		  ORDER BY uploaded_at DESC, pack_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list packs: %w", err)
	}
	defer rows.Close()

	packs := make([]Pack, 0)
	for rows.Next() {
		var p Pack
		if err := rows.Scan(&p.PackID, &p.Name, &p.SHA256, &p.Size, &p.UploadedBy, &p.UploadedAt); err != nil {
			return nil, fmt.Errorf("store: scan pack: %w", err)
		}
		packs = append(packs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list packs: %w", err)
	}
	return packs, nil
}
