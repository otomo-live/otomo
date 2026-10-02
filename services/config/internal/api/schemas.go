// Schema get and replace: design/02-config.md §5 rows GET and PUT
// /namespaces/{ns}/schema. Replacing writes an immutable new version, so the PUT is an
// admin act and the response is the same shape a later GET returns.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// schemasPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift.
const schemasPath = "/api/admin/config/namespaces/{ns}/schema"

// maxSchemaBody bounds a schema document. 1 MiB is generous for a schema and small
// enough that the compiler is not handed an arbitrarily large document.
const maxSchemaBody = 1 << 20

// schemaResponse is the wire shape for one schema version. Schema is a raw JSON object
// so it is embedded rather than encoded as a string.
type schemaResponse struct {
	Namespace     string          `json:"namespace"`
	SchemaVersion int             `json:"schema_version"`
	Schema        json.RawMessage `json:"schema"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

// schemaResponseFrom maps a store row onto the wire shape.
func schemaResponseFrom(s store.Schema) schemaResponse {
	return schemaResponse{
		Namespace:     s.Namespace,
		SchemaVersion: s.SchemaVersion,
		Schema:        json.RawMessage(s.Body),
		CreatedBy:     s.CreatedBy,
		CreatedAt:     s.CreatedAt,
	}
}

// getSchema serves GET /namespaces/{ns}/schema.
func (h *Handlers) getSchema(w http.ResponseWriter, r *http.Request) {
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
	// The ETag is the schema version, so a client can echo it back as If-Match on the
	// next PUT. The double quotes are part of the entity-tag syntax.
	w.Header().Set("ETag", fmt.Sprintf("%q", strconv.Itoa(s.SchemaVersion)))
	writeJSON(w, http.StatusOK, schemaResponseFrom(s))
}

// schemaExpectedVersion reads the schema PUT's optimistic-locking precondition: the
// If-Match header (a quoted or bare positive integer; a weak W/ validator is rejected)
// or the base_version query parameter. Sending both is allowed only when they agree,
// since they are two spellings of the same ticket. present is false when neither was
// supplied. problem is non-empty when a value was supplied but malformed or the two
// disagree; it is a validation message.
func schemaExpectedVersion(r *http.Request) (expected int, present bool, problem string) {
	rawIfMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	rawBase := strings.TrimSpace(r.URL.Query().Get("base_version"))

	headerVersion, headerOK := 0, false
	if rawIfMatch != "" {
		headerVersion, headerOK = parseSchemaVersion(rawIfMatch)
		if !headerOK {
			return 0, true, "If-Match must be a quoted or bare positive integer schema version"
		}
	}
	baseVersion, baseOK := 0, false
	if rawBase != "" {
		baseVersion, baseOK = parseSchemaVersion(rawBase)
		if !baseOK {
			return 0, true, "base_version must be a positive integer schema version"
		}
	}

	switch {
	case headerOK && baseOK:
		if headerVersion != baseVersion {
			return 0, true, fmt.Sprintf("If-Match %q and base_version %d disagree", rawIfMatch, baseVersion)
		}
		return headerVersion, true, ""
	case headerOK:
		return headerVersion, true, ""
	case baseOK:
		return baseVersion, true, ""
	default:
		return 0, false, ""
	}
}

// parseSchemaVersion accepts a bare positive integer or the same wrapped in double
// quotes, and rejects the weak validator prefix W/ and anything else.
func parseSchemaVersion(s string) (int, bool) {
	if strings.HasPrefix(s, "W/") {
		return 0, false
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	} else if strings.ContainsRune(s, '"') {
		return 0, false
	}
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// replaceSchema serves PUT /namespaces/{ns}/schema. The request body IS the schema
// document; it is validated before a new version is written. The version the client
// edited is required as a precondition, so a replace cannot silently overwrite a newer
// schema.
func (h *Handlers) replaceSchema(w http.ResponseWriter, r *http.Request) {
	expected, present, problem := schemaExpectedVersion(r)
	if problem != "" {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", problem)
		return
	}
	if !present {
		WriteError(w, r, http.StatusPreconditionRequired, "precondition_required",
			"send the schema version you edited (If-Match)")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSchemaBody)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "could not read request body")
		return
	}

	if _, err := schema.Compile(raw); err != nil {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			"schema is not a valid JSON Schema: "+describeCompileError(err))
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

	s, err := h.Store.ReplaceSchema(r.Context(), r.PathValue("ns"), raw, &expected, actor)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStaleSchema):
			// CFG-B2: the refusal carries the schema that won, so the editor can show
			// what moved without a second round trip.
			var stale *store.StaleSchemaError
			var details any
			current := 0
			if errors.As(err, &stale) {
				current = stale.Current.SchemaVersion
				details = map[string]any{"schema": schemaResponseFrom(stale.Current)}
			}
			WriteErrorDetails(w, r, http.StatusConflict, "stale_schema",
				fmt.Sprintf("schema v%d was saved since v%d", current, expected), details)
			return
		case errors.Is(err, store.ErrNamespaceNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		}
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, schemaResponseFrom(s))
}

// firstLine returns s up to its first newline and at most max bytes, so a compiler's
// multi-line diagnostic becomes one compact message the client can display.
// describeCompileError names where the schema is wrong. For a meta-schema failure
// that is up to three leaf issues as "at <pointer>: <message>"; the library's own
// first line is a generic headline that locates nothing. Anything else (bad JSON, a
// refused $ref) is short already and is shown as its first line.
func describeCompileError(err error) string {
	issues := schema.Issues(err)
	if len(issues) == 0 {
		return firstLine(err.Error(), 300)
	}
	return firstLine(describeIssues(issues), 300)
}

// describeIssues renders up to three leaf failures as "at <pointer>: <message>",
// joined by "; ". The pointer falls back to "/" for the document root so the text
// always names a location.
func describeIssues(issues []schema.Issue) string {
	parts := make([]string, 0, 3)
	for _, is := range issues {
		if len(parts) == 3 {
			break
		}
		at := is.Pointer
		if at == "" {
			at = "/"
		}
		parts = append(parts, "at "+at+": "+is.Message)
	}
	return strings.Join(parts, "; ")
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}
