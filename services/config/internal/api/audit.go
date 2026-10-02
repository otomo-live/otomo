// Audit feed: design/02-config.md §5 row GET /audit. The Dashboard merges this service's
// entries with other services' into one trail, so the entry shape below is a cross-
// service contract and its field names must not drift.

package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/store"
)

// auditPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift.
const auditPath = "/api/admin/config/audit"

// auditSource is the service name every entry carries, so a merged Dashboard feed can
// attribute an entry to Config.
const auditSource = "config"

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

// listAudit serves GET /audit.
func (h *Handlers) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, ok := parseVersionLimit(q.Get("limit"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("limit must be an integer between 1 and %d", maxVersionLimit))
		return
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

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	entries, next, err := h.Store.ListAudit(r.Context(), query, limit)
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	items := make([]auditEntryItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, auditEntryItem{
			ID:        e.ID,
			At:        e.At,
			ActorID:   e.ActorID,
			ActorName: e.ActorName,
			Source:    auditSource,
			Action:    e.Action,
			Target:    e.Target,
			Details:   e.Details,
		})
	}

	var nextCursor *string
	if next != nil {
		encoded := encodeAuditCursor(*next)
		nextCursor = &encoded
	}
	writeJSON(w, http.StatusOK, auditResponse{Entries: items, NextCursor: nextCursor})
}
