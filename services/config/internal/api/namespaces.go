// Namespace list and create: design/02-config.md §5 rows GET and POST
// /namespaces. Both responses use the item shape defined here, so what the client
// renders after the POST is byte-for-byte what a later GET returns.

package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// namespacesPath is registered in both the route table and Handlers.implemented; it
// is a constant so the two cannot drift.
const namespacesPath = "/api/admin/config/namespaces"

// maxNamespaceBody bounds a create request. 64 KiB is far more than three fields
// need and small enough that a malicious client cannot make the decoder allocate
// arbitrarily.
const maxNamespaceBody = 64 << 10

// namespaceNameRE is the slug rule: dot-separated segments, each starting with a
// lowercase letter and continuing with lowercase letters, digits or underscores.
var namespaceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// namespaceItem is the wire shape for one namespace. LatestVersion is a pointer so it
// serializes as null rather than 0 when nothing is published.
type namespaceItem struct {
	Name          string    `json:"name"`
	Audience      string    `json:"audience"`
	Description   string    `json:"description"`
	LatestVersion *int      `json:"latest_version"`
	CreatedAt     time.Time `json:"created_at"`
	Draft         draftItem `json:"draft"`
}

type draftItem struct {
	Revision              int       `json:"revision"`
	UpdatedAt             time.Time `json:"updated_at"`
	HasUnpublishedChanges bool      `json:"has_unpublished_changes"`
}

// namespacesResponse wraps the list in an object rather than returning a bare array,
// so the envelope can gain pagination or metadata without breaking clients.
type namespacesResponse struct {
	Namespaces []namespaceItem `json:"namespaces"`
}

// createNamespaceRequest is the POST body. Description is optional; the other two are
// required, which is checked explicitly rather than by omitting omitempty.
type createNamespaceRequest struct {
	Name        string `json:"name"`
	Audience    string `json:"audience"`
	Description string `json:"description"`
}

// namespaceItemFrom maps a store row onto the wire shape.
func namespaceItemFrom(ns store.Namespace) namespaceItem {
	return namespaceItem{
		Name:          ns.Name,
		Audience:      ns.Audience,
		Description:   ns.Description,
		LatestVersion: ns.LatestVersion,
		CreatedAt:     ns.CreatedAt,
		Draft: draftItem{
			Revision:              ns.Draft.Revision,
			UpdatedAt:             ns.Draft.UpdatedAt,
			HasUnpublishedChanges: ns.Draft.HasUnpublishedChanges,
		},
	}
}

// validationError names the offending field so the client can highlight it without
// parsing the message.
type validationError struct {
	field   string
	message string
}

// validateCreate applies the §5 field rules in a fixed order, so a body with several
// problems reports the first field a form would present.
func validateCreate(req createNamespaceRequest) *validationError {
	switch {
	case req.Name == "":
		return &validationError{"name", "name is required"}
	case len(req.Name) > 64:
		return &validationError{"name", "name must be at most 64 characters"}
	case !namespaceNameRE.MatchString(req.Name):
		return &validationError{"name", "name must match " + namespaceNameRE.String()}

	case req.Audience == "":
		return &validationError{"audience", "audience is required"}
	case req.Audience != "client" && req.Audience != "server":
		return &validationError{"audience", `audience must be "client" or "server"`}

	case utf8.RuneCountInString(req.Description) > 500:
		return &validationError{"description", "description must be at most 500 characters"}
	}
	return nil
}

// listNamespaces serves GET /namespaces. The list is always a JSON array, empty
// included, because clients should not have to handle null and [] differently.
func (h *Handlers) listNamespaces(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	namespaces, err := h.Store.ListNamespaces(r.Context())
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	items := make([]namespaceItem, 0, len(namespaces))
	for _, ns := range namespaces {
		items = append(items, namespaceItemFrom(ns))
	}
	writeJSON(w, http.StatusOK, namespacesResponse{Namespaces: items})
}

// createNamespace serves POST /namespaces. Auth has already established the caller is
// an admin; this handler only validates the body and maps store outcomes onto status
// codes.
func (h *Handlers) createNamespace(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxNamespaceBody)

	var req createNamespaceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}
	// A second value after the object is trailing data: reject it rather than quietly
	// ignore half the body.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body must contain a single JSON object")
		return
	}

	if ve := validateCreate(req); ve != nil {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", ve.message)
		return
	}

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	var actor store.Entry
	if claims := auth.ClaimsFrom(r.Context()); claims != nil {
		actor = store.Entry{ActorID: claims.Subject, ActorName: claims.ActorName()}
	}

	ns, err := h.Store.CreateNamespace(r.Context(), req.Name, req.Audience, req.Description, actor)
	if err != nil {
		if errors.Is(err, store.ErrNamespaceExists) {
			WriteError(w, r, http.StatusConflict, "already_exists", "a namespace with that name already exists")
			return
		}
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, namespaceItemFrom(ns))
}

// internalError renders the one 500 shape and logs the cause with the request ID. The
// error itself never reaches the client: a DB error can name a table, a constraint or
// a row, none of which is the caller's business.
func (h *Handlers) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if h.Log != nil {
		h.Log.LogAttrs(r.Context(), slog.LevelError, "config request failed",
			slog.String("error", err.Error()),
			slog.String("request_id", RequestID(r.Context())),
		)
	}
	WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}

// writeJSON writes v as the response body. It is the success-path counterpart to
// WriteError and, like it, takes exclusive ownership of the response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
