// Draft read and save: design/02-config.md §5 rows GET and PUT
// /namespaces/{ns}/draft. A draft is the mutable working copy of a namespace's config
// document; a save is guarded by an optimistic-locking revision rather than by
// validating the document, because the version ticket is what turns a draft into a
// validated version.

package api

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// draftsPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift.
const draftsPath = "/api/admin/config/namespaces/{ns}/draft"

// maxDraftBody bounds a save request. 1 MiB matches the schema endpoint and keeps the
// jsonb document small enough that a save cannot be used to bloat the database.
const maxDraftBody = 1 << 20

// draftResponse is the wire shape for one draft. Document is a raw JSON object so it
// is embedded rather than encoded as a string; BaseVersion is a pointer so it
// serializes as null until the first version is cut.
type draftResponse struct {
	Namespace   string          `json:"namespace"`
	Document    json.RawMessage `json:"document"`
	Revision    int             `json:"revision"`
	BaseVersion *int            `json:"base_version"`
	UpdatedBy   string          `json:"updated_by"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// draftResponseFrom maps a store row onto the wire shape.
func draftResponseFrom(d store.Draft) draftResponse {
	return draftResponse{
		Namespace:   d.Namespace,
		Document:    json.RawMessage(d.Body),
		Revision:    d.Revision,
		BaseVersion: d.BaseVersion,
		UpdatedBy:   d.UpdatedBy,
		UpdatedAt:   d.UpdatedAt,
	}
}

// saveDraftRequest is the PUT body. Document is raw so it can be stored without a
// round trip through map[string]any — which would reorder keys and lose nothing here,
// but is an extra parse of a document that is only ever passed through. Revision is a
// pointer so an absent field is distinguishable from an explicit 0.
type saveDraftRequest struct {
	Document json.RawMessage `json:"document"`
	Revision *int            `json:"revision"`
}

// saveDraftResponse is the PUT response: the new revision a client must present next
// time, and when it landed.
type saveDraftResponse struct {
	Revision  int       `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// getDraft serves GET /namespaces/{ns}/draft.
func (h *Handlers) getDraft(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	d, err := h.Store.Draft(r.Context(), r.PathValue("ns"))
	if err != nil {
		if errors.Is(err, store.ErrNamespaceNotFound) {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		}
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, draftResponseFrom(d))
}

// saveDraft serves PUT /namespaces/{ns}/draft.
//
// The draft is deliberately NOT validated against the namespace's schema here: a draft
// is work in progress, so a half-finished document has to be storable and revisable.
// The version ticket enforces validity, and only a version is ever served to players.
func (h *Handlers) saveDraft(w http.ResponseWriter, r *http.Request) {
	var req saveDraftRequest
	if !readDraftBody(w, r, &req) {
		return
	}

	if ve := validateSaveDraft(req); ve != nil {
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

	saved, err := h.Store.SaveDraft(r.Context(), r.PathValue("ns"), req.Document, *req.Revision, actor)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStaleRevision):
			// CFG-B3: the refusal carries the draft as it now is, so the editor can
			// show what moved without a second round trip. Best effort: if the read
			// fails the 409 still goes out, without details.
			var details any
			if current, err := h.Store.Draft(r.Context(), r.PathValue("ns")); err == nil {
				details = map[string]any{"draft": draftResponseFrom(current)}
			}
			WriteErrorDetails(w, r, http.StatusConflict, "stale_revision",
				fmt.Sprintf("the draft has moved on since revision %d", *req.Revision), details)
			return
		case errors.Is(err, store.ErrNamespaceNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		}
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, saveDraftResponse{Revision: saved.Revision, UpdatedAt: saved.UpdatedAt})
}

// readDraftBody reads and decodes a draft-family request body into dst under the rules
// shared by the draft PUT and the draft validate POST: a 1 MiB cap, exactly one JSON
// object, no duplicate members, and no unknown fields. It writes the error response
// itself and reports whether the caller should carry on; on failure dst is untouched.
func readDraftBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	return readStrictBody(w, r, dst, maxDraftBody)
}

// readStrictBody reads and decodes a request body into dst: a max-byte cap, exactly one
// JSON object, no duplicate members, and no unknown fields. It writes the error
// response itself and reports whether the caller should carry on; on failure dst is
// untouched.
func readStrictBody(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "could not read request body")
		return false
	}

	// A plain json.Decoder accepts duplicate object member names and invalid UTF-8;
	// jsontext rejects both, and neither is legal JSON.
	if !jsontext.Value(raw).IsValid() {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return false
	}
	// A second value after the object is trailing data: reject it rather than quietly
	// ignore half the body.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body must contain a single JSON object")
		return false
	}
	return true
}

// validateSaveDraft applies the §5 field rules in a fixed order, so a body with
// several problems reports the first field a form would present.
func validateSaveDraft(req saveDraftRequest) *validationError {
	if ve := validateDocument(req.Document); ve != nil {
		return ve
	}
	switch {
	case req.Revision == nil:
		return &validationError{"revision", "revision is required"}
	case *req.Revision < 1:
		return &validationError{"revision", "revision must be at least 1"}
	}
	return nil
}

// validateDocument applies the document field rules shared by the draft PUT and the
// draft validate POST.
func validateDocument(raw json.RawMessage) *validationError {
	switch {
	case len(raw) == 0:
		return &validationError{"document", "document is required"}
	case !isJSONObject(raw):
		return &validationError{"document", "document must be a JSON object"}
	}
	return nil
}

// isJSONObject reports whether raw (already known to be valid JSON) is an object. It
// skips leading whitespace rather than decoding, because decoding a JSON null into a
// map is indistinguishable from decoding nothing.
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == '{'
		}
	}
	return false
}
