// Staff routes (SE-7): player lookup, force-disband and the audit feed, under
// /api/admin/session with PHP Admin Auth's tokens. The guard has already checked the
// route's minimum role: viewer for the reads, live_ops for force-disband.

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/auth"
	"github.com/otomo-live/otomo/services/session/internal/store"
)

// auditSource is the service name every audit entry carries, so the Dashboard's merged
// trail can attribute it to Session.
const auditSource = "session"

// The audit feed's page size: 50 by default, at most 200, as Config's.
const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

// maxDisbandBody caps a force-disband body, {"reason"}, and maxDisbandReason its reason.
const (
	maxDisbandBody   = 1 << 10
	maxDisbandReason = 500
)

// AdminStore is the part of the store the staff routes use. *store.DB implements it.
type AdminStore interface {
	LookupPlayers(ctx context.Context, name string, discriminator *int) ([]store.Profile, error)
	GetProfile(ctx context.Context, playerID uuid.UUID) (store.Profile, error)
	GetParty(ctx context.Context, player string) (*store.Party, error)
	ForceDisband(ctx context.Context, partyID string, actor store.Entry) (*store.Party, []store.Notice, error)
	ListAudit(ctx context.Context, q store.AuditQuery, limit int) ([]store.AuditRecord, *int64, error)
}

type playerSummary struct {
	PlayerID      string `json:"player_id"`
	DisplayName   string `json:"display_name"`
	Discriminator int    `json:"discriminator"`
}

// playerDetail is GET /players/{id}: the profile, presence and party.
type playerDetail struct {
	playerSummary
	Status string         `json:"status,omitempty"`
	Party  *partyResponse `json:"party"`
}

type auditItem struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorID   string          `json:"actor_id"`
	ActorName string          `json:"actor_name"`
	Source    string          `json:"source"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Details   json.RawMessage `json:"details"`
}

type auditPage struct {
	Entries    []auditItem `json:"entries"`
	NextCursor *string     `json:"next_cursor"`
}

func (h *Handlers) adminStoreReady(w http.ResponseWriter, r *http.Request) bool {
	if h.Admin == nil {
		h.internalError(w, r, errors.New("no admin store configured"))
		return false
	}
	return true
}

// lookupPlayers serves GET /players?name=&discriminator=: every player with that display
// name (without case), or the one with that name and discriminator.
func (h *Handlers) lookupPlayers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "name is required")
		return
	}
	var disc *int
	if raw := q.Get("discriminator"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 9999 {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "discriminator must be a number from 1 to 9999")
			return
		}
		disc = &n
	}
	if !h.adminStoreReady(w, r) {
		return
	}
	found, err := h.Admin.LookupPlayers(r.Context(), name, disc)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := struct {
		Players []playerSummary `json:"players"`
	}{Players: make([]playerSummary, 0, len(found))}
	for _, p := range found {
		out.Players = append(out.Players, playerSummary{p.PlayerID.String(), p.DisplayName, p.Discriminator})
	}
	writeJSON(w, http.StatusOK, out)
}

// getPlayer serves GET /players/{id}: the profile, presence status, and party (null when
// the player is in none).
func (h *Handlers) getPlayer(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "id must be a player UUID")
		return
	}
	if !h.adminStoreReady(w, r) {
		return
	}
	p, err := h.Admin.GetProfile(r.Context(), id)
	if errors.Is(err, store.ErrProfileNotFound) {
		WriteError(w, r, http.StatusNotFound, "player_not_found", "no such player")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := playerDetail{playerSummary: playerSummary{p.PlayerID.String(), p.DisplayName, p.Discriminator}}
	party, err := h.Admin.GetParty(r.Context(), id.String())
	switch {
	case errors.Is(err, store.ErrNotInParty):
	case err != nil:
		h.internalError(w, r, err)
		return
	default:
		resp := h.partyFrom(party)
		h.withStatuses(r, &resp)
		out.Party = &resp
	}
	if h.Presence != nil {
		if st, err := h.Presence.Statuses(r.Context(), []string{id.String()}); err == nil {
			out.Status = st[id.String()]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// forceDisband serves POST /parties/{party_id}/disband, optionally with {"reason"}:
// live_ops. The party is deleted in any state and the audit row is written in the same
// transaction (SES-A5). A running match is not cancelled; the Allocator's later
// callback finds no party and is answered 204 (doc 14 §2 note 2). Every member gets
// party.disbanded. It answers 204, or 404 party_not_found.
func (h *Handlers) forceDisband(w http.ResponseWriter, r *http.Request) {
	partyID, ok := pathID(w, r, "party_id", "validation_failed")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if !decodeBody(w, r, maxDisbandBody, &req) {
			return
		}
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len([]rune(req.Reason)) > maxDisbandReason {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("reason must be at most %d characters", maxDisbandReason))
		return
	}
	if !h.adminStoreReady(w, r) {
		return
	}
	who := auth.IdentityFrom(r.Context())
	if who == nil {
		h.internalError(w, r, errors.New("force-disband reached without a verified identity"))
		return
	}
	actor := store.Entry{ActorID: who.Subject, ActorName: who.ActorName()}
	if req.Reason != "" {
		actor.Details = map[string]any{"reason": req.Reason}
	}
	_, notices, err := h.Admin.ForceDisband(r.Context(), partyID, actor)
	if errors.Is(err, store.ErrPartyNotFound) {
		WriteError(w, r, http.StatusNotFound, "party_not_found", "no such party")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	h.publish(r, notices)
	w.WriteHeader(http.StatusNoContent)
}

// listAudit serves GET /audit: Session's staff actions, newest first, in the shape
// Config's feed uses, so the Dashboard merges the two without translating. Parameters:
// limit (1 to 200, default 50), cursor (opaque, from next_cursor), actor (id or name),
// action, from and to (RFC 3339).
func (h *Handlers) listAudit(w http.ResponseWriter, r *http.Request) {
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
	var query store.AuditQuery
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
	for _, f := range []struct {
		name string
		dst  **time.Time
	}{{"from", &query.From}, {"to", &query.To}} {
		if v := q.Get(f.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, "validation_failed", f.name+" must be an RFC3339 timestamp")
				return
			}
			*f.dst = &t
		}
	}
	if !h.adminStoreReady(w, r) {
		return
	}
	entries, next, err := h.Admin.ListAudit(r.Context(), query, limit)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := auditPage{Entries: make([]auditItem, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, auditItem{
			ID: e.ID, At: e.At, ActorID: e.ActorID, ActorName: e.ActorName,
			Source: auditSource, Action: e.Action, Target: e.Target, Details: e.Details,
		})
	}
	if next != nil {
		c := encodeAuditCursor(*next)
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}

// The cursor is Config's: base64url, unpadded, of {"before":id}.
type auditCursor struct {
	Before int64 `json:"before"`
}

func encodeAuditCursor(id int64) string {
	body, _ := json.Marshal(auditCursor{Before: id})
	return base64.RawURLEncoding.EncodeToString(body)
}

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
