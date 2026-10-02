// Version creation: design/02-config.md §5 row POST /namespaces/{ns}/versions. Cutting a
// version validates the working draft against the namespace's current schema,
// canonicalises it, writes the canonical bytes to the blob store and records the
// immutable snapshot — all behind one transaction so a version never points at a blob
// that is not there.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/diff"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// versionsPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift. versionPath and diffPath sit beside it for the same
// reason.
const versionsPath = "/api/admin/config/namespaces/{ns}/versions"

// versionPath reads one version.
const versionPath = versionsPath + "/{v}"

// diffPath compares two versions, or a version and the working draft.
const diffPath = "/api/admin/config/namespaces/{ns}/diff"

// History paging bounds. A client that asks for more than maxVersionLimit gets a 400
// rather than a silently clamped page, so it can tell its request was not honoured.
const (
	defaultVersionLimit = 50
	maxVersionLimit     = 200
)

// maxVersionBody bounds a create-version request. 64 KiB is far more than a message and
// a revision need.
const maxVersionBody = 64 << 10

// maxVersionMessage is the longest message a version may carry.
const maxVersionMessage = 500

// createVersionRequest is the POST body. Revision is a pointer so an absent field is
// distinguishable from an explicit 0.
type createVersionRequest struct {
	Message  string `json:"message"`
	Revision *int   `json:"revision"`
}

// versionResponse is the wire shape for one version.
type versionResponse struct {
	Namespace     string    `json:"namespace"`
	Version       int       `json:"version"`
	SchemaVersion int       `json:"schema_version"`
	SHA256        string    `json:"sha256"`
	Size          int       `json:"size"`
	Message       string    `json:"message"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// versionResponseFrom maps a store row onto the wire shape.
func versionResponseFrom(v store.Version) versionResponse {
	return versionResponse{
		Namespace:     v.Namespace,
		Version:       v.Version,
		SchemaVersion: v.SchemaVersion,
		SHA256:        v.SHA256,
		Size:          v.Size,
		Message:       v.Message,
		CreatedBy:     v.CreatedBy,
		CreatedAt:     v.CreatedAt,
	}
}

// schemaIssuesError carries the leaf failures a draft produced against one schema
// version. It is the store's prepare callback's way of telling the handler that the
// error is about the document, not about the service, so it becomes a 400.
type schemaIssuesError struct {
	schemaVersion int
	issues        []schema.Issue
}

func (e *schemaIssuesError) Error() string {
	return fmt.Sprintf("draft does not satisfy schema version %d", e.schemaVersion)
}

// message is the client-facing text: the schema version the answer is relative to and
// up to three located failures.
func (e *schemaIssuesError) message() string {
	return fmt.Sprintf("draft does not satisfy schema version %d: %s",
		e.schemaVersion, describeIssues(e.issues))
}

// createVersion serves POST /namespaces/{ns}/versions. Auth has already established the
// caller is live_ops.
func (h *Handlers) createVersion(w http.ResponseWriter, r *http.Request) {
	var req createVersionRequest
	if !readStrictBody(w, r, &req, maxVersionBody) {
		return
	}

	if ve := validateCreateVersion(req); ve != nil {
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

	ns := r.PathValue("ns")
	prepare := func(draftBody []byte, schemaVersion int, schemaBody []byte) ([]byte, string, error) {
		compiled, err := h.Schemas.Get(ns, schemaVersion, schemaBody)
		if err != nil {
			// A stored schema that will not compile is this service's fault, not the
			// caller's.
			return nil, "", fmt.Errorf("compile stored schema: %w", err)
		}

		issues, err := schema.Validate(compiled, draftBody)
		if err != nil {
			return nil, "", err
		}
		if len(issues) > 0 {
			return nil, "", &schemaIssuesError{schemaVersion: schemaVersion, issues: issues}
		}

		canonical, err := schema.Canonical(draftBody)
		if err != nil {
			return nil, "", err
		}

		sum := sha256.Sum256(canonical)
		sha := hex.EncodeToString(sum[:])

		if h.Blobs == nil {
			return nil, "", errors.New("no blob store configured")
		}
		ref, err := h.Blobs.PutBytes(r.Context(), canonical)
		if err != nil {
			return nil, "", err
		}
		if ref.SHA256 != sha {
			return nil, "", fmt.Errorf("blob store stored %s, computed %s", ref.SHA256, sha)
		}
		return canonical, sha, nil
	}

	v, err := h.Store.CreateVersion(r.Context(), ns, *req.Revision, req.Message, actor, prepare)
	if err != nil {
		var issues *schemaIssuesError
		switch {
		case errors.As(err, &issues):
			if h.ValidationFailed != nil {
				h.ValidationFailed()
			}
			WriteError(w, r, http.StatusBadRequest, "validation_failed", issues.message())
			return
		case errors.Is(err, store.ErrNamespaceNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		case errors.Is(err, store.ErrStaleRevision):
			WriteError(w, r, http.StatusConflict, "stale_revision",
				fmt.Sprintf("the draft has moved on since revision %d", *req.Revision))
			return
		case errors.Is(err, store.ErrNoChanges):
			latest := 0
			var noChanges *store.NoChangesError
			if errors.As(err, &noChanges) {
				latest = noChanges.Version
			}
			WriteError(w, r, http.StatusConflict, "no_changes",
				fmt.Sprintf("the draft is identical to version %d", latest))
			return
		}
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, versionResponseFrom(v))
}

// validateCreateVersion applies the field rules in a fixed order, so a body with
// several problems reports the first field a form would present.
func validateCreateVersion(req createVersionRequest) *validationError {
	switch {
	case strings.TrimSpace(req.Message) == "":
		return &validationError{"message", "message is required"}
	case utf8.RuneCountInString(req.Message) > maxVersionMessage:
		return &validationError{"message", fmt.Sprintf("message must be at most %d characters", maxVersionMessage)}
	case req.Revision == nil:
		return &validationError{"revision", "revision is required"}
	case *req.Revision < 1:
		return &validationError{"revision", "revision must be at least 1"}
	}
	return nil
}

// versionListItem is one row of the history list. The body and its canonical size are
// deliberately absent.
type versionListItem struct {
	Version       int       `json:"version"`
	SchemaVersion int       `json:"schema_version"`
	SHA256        string    `json:"sha256"`
	Message       string    `json:"message"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// versionListResponse wraps the page. NextBefore is a pointer so the last page carries
// an explicit null rather than 0.
type versionListResponse struct {
	Versions   []versionListItem `json:"versions"`
	NextBefore *int              `json:"next_before"`
}

// versionDetailResponse is one version plus its canonical size and full document.
type versionDetailResponse struct {
	Version       int             `json:"version"`
	SchemaVersion int             `json:"schema_version"`
	SHA256        string          `json:"sha256"`
	Size          int             `json:"size"`
	Message       string          `json:"message"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	Document      json.RawMessage `json:"document"`
}

// diffSide names one end of a diff and carries its full document.
type diffSide struct {
	Ref      string          `json:"ref"`
	Document json.RawMessage `json:"document"`
}

// diffResponse is the diff wire shape. Changes is always an array, empty when the two
// documents are identical.
type diffResponse struct {
	From    diffSide      `json:"from"`
	To      diffSide      `json:"to"`
	Changes []diff.Change `json:"changes"`
}

// listVersions serves GET /namespaces/{ns}/versions.
func (h *Handlers) listVersions(w http.ResponseWriter, r *http.Request) {
	before, ok := parseBefore(r.URL.Query().Get("before"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "before must be a positive integer")
		return
	}
	limit, ok := parseVersionLimit(r.URL.Query().Get("limit"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("limit must be an integer between 1 and %d", maxVersionLimit))
		return
	}

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	versions, next, err := h.Store.ListVersions(r.Context(), r.PathValue("ns"), before, limit)
	if err != nil {
		if errors.Is(err, store.ErrNamespaceNotFound) {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace")
			return
		}
		h.internalError(w, r, err)
		return
	}

	items := make([]versionListItem, 0, len(versions))
	for _, v := range versions {
		items = append(items, versionListItem{
			Version:       v.Version,
			SchemaVersion: v.SchemaVersion,
			SHA256:        v.SHA256,
			Message:       v.Message,
			CreatedBy:     v.CreatedBy,
			CreatedAt:     v.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, versionListResponse{Versions: items, NextBefore: next})
}

// getVersion serves GET /namespaces/{ns}/versions/{v}. Size is computed from the
// canonical bytes, not from the jsonb text Postgres stores, because key order and
// float formatting in that text are not canonical.
func (h *Handlers) getVersion(w http.ResponseWriter, r *http.Request) {
	version, ok := parseVersionNumber(r.PathValue("v"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "version must be a positive integer")
		return
	}

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	v, err := h.Store.GetVersion(r.Context(), r.PathValue("ns"), version)
	if err != nil {
		if errors.Is(err, store.ErrVersionNotFound) {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such version")
			return
		}
		h.internalError(w, r, err)
		return
	}

	canonical, err := schema.Canonical(v.Body)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("canonicalize stored version: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, versionDetailResponse{
		Version:       v.Version,
		SchemaVersion: v.SchemaVersion,
		SHA256:        v.SHA256,
		Size:          len(canonical),
		Message:       v.Message,
		CreatedBy:     v.CreatedBy,
		CreatedAt:     v.CreatedAt,
		Document:      json.RawMessage(canonical),
	})
}

// diffVersions serves GET /namespaces/{ns}/diff. Both refs must be present and each is
// a positive version number or the literal "draft"; both full documents are returned so
// the UI can render a richer view than the change list alone.
func (h *Handlers) diffVersions(w http.ResponseWriter, r *http.Request) {
	fromRef, fromVersion, ok := parseDiffRef(r.URL.Query().Get("from"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "from must be a positive version or draft")
		return
	}
	toRef, toVersion, ok := parseDiffRef(r.URL.Query().Get("to"))
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "to must be a positive version or draft")
		return
	}

	if h.Store == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	ns := r.PathValue("ns")
	fromDoc, err := h.resolveDocument(r.Context(), ns, fromVersion)
	if err != nil {
		h.writeDocumentError(w, r, err)
		return
	}
	toDoc, err := h.resolveDocument(r.Context(), ns, toVersion)
	if err != nil {
		h.writeDocumentError(w, r, err)
		return
	}

	changes, err := diff.Diff(fromDoc, toDoc)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("diff documents: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, diffResponse{
		From:    diffSide{Ref: fromRef, Document: fromDoc},
		To:      diffSide{Ref: toRef, Document: toDoc},
		Changes: changes,
	})
}

// resolveDocument loads the document a diff ref names: the working draft when version
// is nil, otherwise that immutable version.
func (h *Handlers) resolveDocument(ctx context.Context, ns string, version *int) (json.RawMessage, error) {
	if version == nil {
		d, err := h.Store.Draft(ctx, ns)
		if err != nil {
			return nil, err
		}
		return d.Body, nil
	}
	v, err := h.Store.GetVersion(ctx, ns, *version)
	if err != nil {
		return nil, err
	}
	return v.Body, nil
}

// writeDocumentError maps the two not-found sentinels a document lookup can return onto
// a 404 and everything else onto the internal error shape.
func (h *Handlers) writeDocumentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNamespaceNotFound) || errors.Is(err, store.ErrVersionNotFound) {
		WriteError(w, r, http.StatusNotFound, "not_found", "no such namespace or version")
		return
	}
	h.internalError(w, r, err)
}

// parseBefore reads the optional before cursor. An absent or empty value is no cursor;
// anything that is not a positive integer is a validation error.
func parseBefore(raw string) (*int, bool) {
	if raw == "" {
		return nil, true
	}
	n, ok := parseVersionNumber(raw)
	if !ok {
		return nil, false
	}
	return &n, true
}

// parseVersionLimit reads the optional limit, defaulting to defaultVersionLimit and
// rejecting anything outside 1..maxVersionLimit.
func parseVersionLimit(raw string) (int, bool) {
	if raw == "" {
		return defaultVersionLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxVersionLimit {
		return 0, false
	}
	return n, true
}

// parseVersionNumber parses a positive decimal version number.
func parseVersionNumber(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// parseDiffRef parses one end of a diff. It returns the ref as it appears in the
// response, the version to resolve (nil for the draft) and whether the value was valid.
func parseDiffRef(raw string) (ref string, version *int, ok bool) {
	if raw == "draft" {
		return "draft", nil, true
	}
	n, ok := parseVersionNumber(raw)
	if !ok {
		return "", nil, false
	}
	return strconv.Itoa(n), &n, true
}
