// GET /admin-auth/audit — the staff audit feed.
//
// The Dashboard merges this feed with Config's GET /api/admin/config/audit into one
// trail, so the wire shape below is a cross-service contract and its field names must
// not drift from services/config/internal/api/audit.go.

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
)

// Audit limits. A page defaults to 50 entries and may not exceed 200.
const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

// auditSource is the service name every entry carries, so a merged Dashboard feed can
// attribute an entry to admin-auth.
const auditSource = "admin-auth"

// AuditStore is the subset of *store.DB the audit handler needs.
type AuditStore interface {
	ListAudit(ctx context.Context, q store.AuditQuery, limit int) ([]store.AuditRecord, *int64, error)
}

// AuditDeps is everything the audit handler needs.
type AuditDeps struct {
	Store  AuditStore
	Logger *slog.Logger
}

// auditEntryItem is one audit entry on the wire. Details is embedded as a JSON object
// rather than a string so the Dashboard can render it directly.
type auditEntryItem struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorID   string          `json:"actor_id"`
	ActorName string          `json:"actor_name"`
	Source    string          `json:"source"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Details   json.RawMessage `json:"details"`
}

// auditResponse wraps a page. NextCursor is an opaque token a client passes back as
// cursor; it is a pointer so the last page carries an explicit null.
type auditResponse struct {
	Entries    []auditEntryItem `json:"entries"`
	NextCursor *string          `json:"next_cursor"`
}

// auditCursor is the decoded form of the opaque cursor: the id the next page must be
// strictly below.
type auditCursor struct {
	Before int64 `json:"before"`
}

// encodeAuditCursor turns an id into the opaque cursor: base64url of {"before":id}, with
// no padding so it is safe in a query string.
func encodeAuditCursor(id int64) string {
	body, _ := json.Marshal(auditCursor{Before: id})
	return base64.RawURLEncoding.EncodeToString(body)
}

// decodeAuditCursor parses a cursor, rejecting anything that is not the documented
// shape. A malformed cursor is a 400 rather than an ignored parameter, so a client can
// tell paging is broken instead of silently looping over page one.
func decodeAuditCursor(raw string) (int64, error) {
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, fmt.Errorf("cursor is not base64url: %w", err)
	}
	var c auditCursor
	if err := json.Unmarshal(body, &c); err != nil {
		return 0, fmt.Errorf("cursor is not a JSON object: %w", err)
	}
	if c.Before < 1 {
		return 0, errors.New("cursor before must be at least 1")
	}
	return c.Before, nil
}

// Audit returns the GET /admin-auth/audit handler. The server's requireRole("viewer")
// middleware has already verified the bearer and loaded an active caller.
func Audit(deps AuditDeps) http.HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		limit := defaultAuditLimit
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > maxAuditLimit {
				WriteError(w, r, http.StatusBadRequest, "validation_failed",
					fmt.Sprintf("limit must be an integer between 1 and %d", maxAuditLimit))
				return
			}
			limit = n
		}

		query := store.AuditQuery{}
		if raw := q.Get("cursor"); raw != "" {
			before, err := decodeAuditCursor(raw)
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, "validation_failed", "cursor is malformed")
				return
			}
			query.Before = &before
		}
		if v := q.Get("action"); v != "" {
			query.Action = &v
		}
		if v := q.Get("actor"); v != "" {
			query.Actor = &v
		}
		if v := q.Get("from"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, "validation_failed", "from must be an RFC3339 timestamp")
				return
			}
			query.From = &t
		}
		if v := q.Get("to"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, "validation_failed", "to must be an RFC3339 timestamp")
				return
			}
			query.To = &t
		}

		entries, next, err := deps.Store.ListAudit(r.Context(), query, limit)
		if err != nil {
			internalError(w, r, log, "list audit", err)
			return
		}

		items := make([]auditEntryItem, 0, len(entries))
		for _, e := range entries {
			details := e.Details
			if len(details) == 0 {
				details = json.RawMessage(`{}`)
			}
			items = append(items, auditEntryItem{
				ID:        e.ID,
				At:        e.At,
				ActorID:   e.ActorID,
				ActorName: e.ActorName,
				Source:    auditSource,
				Action:    e.Action,
				Target:    e.Target,
				Details:   details,
			})
		}

		var nextCursor *string
		if next != nil {
			encoded := encodeAuditCursor(*next)
			nextCursor = &encoded
		}
		writeJSON(w, http.StatusOK, auditResponse{Entries: items, NextCursor: nextCursor})
	}
}
