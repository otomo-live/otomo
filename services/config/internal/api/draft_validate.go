// Draft validation: design/02-config.md §5 row POST
// /namespaces/{ns}/draft/validate. It answers whether a draft document would satisfy
// the namespace's current schema, without saving anything and without cutting a
// version: it is the feedback loop a draft editor shows while the operator types.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// draftValidatePath is registered in both the route table and Handlers.implemented; it
// is a constant so the two cannot drift.
const draftValidatePath = "/api/admin/config/namespaces/{ns}/draft/validate"

// validateDraftRequest is the POST body. Document is raw so it reaches the validator
// without a round trip through map[string]any, which would reorder keys and coerce
// numbers.
type validateDraftRequest struct {
	Document json.RawMessage `json:"document"`
}

// validateDraftResponse names the schema version the answer is relative to, so a client
// editing against a stale schema can tell. Errors is always present, never null.
type validateDraftResponse struct {
	Valid         bool           `json:"valid"`
	SchemaVersion int            `json:"schema_version"`
	Errors        []schema.Issue `json:"errors"`
}

// validateDraft serves POST /namespaces/{ns}/draft/validate. Auth has already
// established the caller is live_ops. The document is checked against the namespace's
// latest schema; nothing is written.
func (h *Handlers) validateDraft(w http.ResponseWriter, r *http.Request) {
	var req validateDraftRequest
	if !readDraftBody(w, r, &req) {
		return
	}

	if ve := validateDocument(req.Document); ve != nil {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", ve.message)
		return
	}

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	s, err := h.Store.LatestSchema(r.Context(), r.PathValue("ns"))
	if err != nil {
		if errors.Is(err, store.ErrNamespaceNotFound) {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		}
		h.internalError(w, r, err)
		return
	}

	compiled, err := h.Schemas.Get(s.Namespace, s.SchemaVersion, s.Body)
	if err != nil {
		// A stored schema that will not compile is this service's fault, not the
		// caller's: the schema was accepted (or seeded) when it was written.
		h.internalError(w, r, fmt.Errorf("compile stored schema: %w", err))
		return
	}

	issues, err := schema.Validate(compiled, req.Document)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "document is not valid JSON")
		return
	}
	if issues == nil {
		issues = []schema.Issue{}
	}
	if len(issues) > 0 && h.ValidationFailed != nil {
		h.ValidationFailed()
	}
	writeJSON(w, http.StatusOK, validateDraftResponse{
		Valid:         len(issues) == 0,
		SchemaVersion: s.SchemaVersion,
		Errors:        issues,
	})
}
