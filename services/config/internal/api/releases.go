// Release publishing: design/02-config.md §5 row POST /channels/{ch}/releases. A publish
// selects one version per namespace plus any number of content packs and freezes them
// into a channel's client manifest, plus a server manifest carrying the server-audience
// namespaces. The store's Publish does the transactional work; this file owns the wire
// shape and the field rules.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// releasesPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift.
const releasesPath = "/api/admin/config/channels/{ch}/releases"

// liveReleasesPath is the literal route the table uses to raise a live publish to
// admin. ServeMux keys handlers by pattern, so the handler below is registered under
// both this pattern and releasesPath.
const liveReleasesPath = "/api/admin/config/channels/live/releases"

// rollbackPath and promotePath are the two admin release mutations; both are registered
// in the route table and here so the pattern string cannot drift.
const (
	rollbackPath = "/api/admin/config/channels/{ch}/rollback"
	promotePath  = "/api/admin/config/channels/{ch}/promote"
)

// maxReleaseBody bounds a rollback or promote request. Both carry at most two ids and a
// message, so 64 KiB is generous.
const maxReleaseBody = 64 << 10

// maxPublishBody bounds a publish request. 64 KiB is far more than a selection of
// versions, packs and a message needs.
const maxPublishBody = 64 << 10

// maxPublishMessage is the longest message a release may carry.
const maxPublishMessage = 500

// minClientVersionRE is the §5 grammar for a minimum client version: three dot-separated
// decimal components.
var minClientVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// packSHA256RE is the only spelling of a pack hash this service accepts. It mirrors
// blob.validateHash so a request cannot name a blob the store would reject.
var packSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// publishVersionRequest is one entry of the request's "versions" array.
type publishVersionRequest struct {
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
}

// publishPackRequest is one entry of the request's "packs" array. Content is addressed
// by hash, so the name is the store's, not the caller's.
type publishPackRequest struct {
	SHA256 string `json:"sha256"`
}

// publishRequest is the POST body. Versions and Packs are pointers to slices so an
// absent field is distinguishable from a present empty array: both may be empty, but
// neither may be missing.
type publishRequest struct {
	BaseReleaseID    *int64                   `json:"base_release_id"`
	Versions         *[]publishVersionRequest `json:"versions"`
	Packs            *[]publishPackRequest    `json:"packs"`
	MinClientVersion string                   `json:"min_client_version"`
	Message          string                   `json:"message"`
}

// releaseResponse is the wire shape for a published release. Manifest and
// ServerManifest are the canonical documents themselves, embedded rather than
// stringified; the hashes are null for a release written before server manifests
// existed.
type releaseResponse struct {
	ReleaseID            int64           `json:"release_id"`
	Channel              string          `json:"channel"`
	ManifestSHA256       string          `json:"manifest_sha256"`
	MinClientVersion     string          `json:"min_client_version"`
	Message              string          `json:"message"`
	CreatedBy            string          `json:"created_by"`
	CreatedAt            time.Time       `json:"created_at"`
	Manifest             json.RawMessage `json:"manifest"`
	ServerManifestSHA256 *string         `json:"server_manifest_sha256"`
	ServerManifest       json.RawMessage `json:"server_manifest"`
}

// optionalSHA maps the store's "" (a release written before server manifests existed)
// onto JSON null, so a client can tell "no server manifest" from a hash.
func optionalSHA(sha string) *string {
	if sha == "" {
		return nil
	}
	return &sha
}

// releaseResponseFrom maps a store row onto the wire shape. A release with no server
// manifest (a legacy row) serialises both server fields as null rather than with a
// zero-value string.
func releaseResponseFrom(rel store.Release) releaseResponse {
	return releaseResponse{
		ReleaseID:            rel.ReleaseID,
		Channel:              rel.Channel,
		ManifestSHA256:       rel.ManifestSHA256,
		MinClientVersion:     rel.MinClientVersion,
		Message:              rel.Message,
		CreatedBy:            rel.CreatedBy,
		CreatedAt:            rel.CreatedAt,
		Manifest:             rel.Manifest,
		ServerManifestSHA256: optionalSHA(rel.ServerManifestSHA256),
		ServerManifest:       rel.ServerManifest,
	}
}

// publishRelease serves POST /channels/{ch}/releases. Auth has already established the
// caller's role; for the live literal route the path has no {ch} segment, so the
// channel is implied.
func (h *Handlers) publishRelease(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	if ch == "" {
		ch = "live"
	}

	var req publishRequest
	if !readStrictBody(w, r, &req, maxPublishBody) {
		return
	}

	if ve := validatePublish(ch, req); ve != nil {
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

	versions := make([]store.PublishVersion, 0, len(*req.Versions))
	for _, v := range *req.Versions {
		versions = append(versions, store.PublishVersion{Namespace: v.Namespace, Version: v.Version})
	}
	packs := make([]string, 0, len(*req.Packs))
	for _, p := range *req.Packs {
		packs = append(packs, p.SHA256)
	}

	rel, err := h.Store.Publish(r.Context(), ch, store.PublishRequest{
		BaseReleaseID:    *req.BaseReleaseID,
		Versions:         versions,
		PackSHA256:       packs,
		MinClientVersion: req.MinClientVersion,
		Message:          req.Message,
	}, actor, h.Blobs)
	if err != nil {
		var stale *store.StaleReleaseError
		var unknown *store.UnknownTargetError
		switch {
		case errors.As(err, &stale):
			WriteError(w, r, http.StatusConflict, "stale_release",
				fmt.Sprintf("channel %s has moved from release %d to %d", stale.Channel, stale.Base, stale.Current))
			return
		case errors.As(err, &unknown):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such "+unknown.Kind+": "+unknown.Name)
			return
		}
		h.internalError(w, r, err)
		return
	}

	// Only a successful Publish is a publish; rollback and promote have their own paths
	// and never call this seam.
	if h.Published != nil {
		h.Published(ch)
	}
	writeJSON(w, http.StatusCreated, releaseResponseFrom(rel))
}

// validatePublish applies the field rules in a fixed order, so a body with several
// problems reports the first field a form would present.
func validatePublish(ch string, req publishRequest) *validationError {
	switch {
	case ch != "dev" && ch != "staging" && ch != "live":
		return &validationError{"channel", "channel must be one of dev, staging or live"}
	case req.BaseReleaseID == nil:
		return &validationError{"base_release_id", "base_release_id is required"}
	case *req.BaseReleaseID < 1:
		return &validationError{"base_release_id", "base_release_id must be at least 1"}
	case !minClientVersionRE.MatchString(req.MinClientVersion):
		return &validationError{"min_client_version", "min_client_version must match " + minClientVersionRE.String()}
	case strings.TrimSpace(req.Message) == "":
		return &validationError{"message", "message is required"}
	case utf8.RuneCountInString(req.Message) > maxPublishMessage:
		return &validationError{"message", fmt.Sprintf("message must be at most %d characters", maxPublishMessage)}
	case req.Versions == nil:
		return &validationError{"versions", "versions is required"}
	case req.Packs == nil:
		return &validationError{"packs", "packs is required"}
	}

	seenNS := make(map[string]bool, len(*req.Versions))
	for _, v := range *req.Versions {
		if seenNS[v.Namespace] {
			return &validationError{"namespace", "versions contains duplicate namespace " + v.Namespace}
		}
		seenNS[v.Namespace] = true
	}

	seenSHA := make(map[string]bool, len(*req.Packs))
	for _, p := range *req.Packs {
		if !packSHA256RE.MatchString(p.SHA256) {
			return &validationError{"sha256", "packs sha256 must match " + packSHA256RE.String()}
		}
		if seenSHA[p.SHA256] {
			return &validationError{"sha256", "packs contains duplicate sha256 " + p.SHA256}
		}
		seenSHA[p.SHA256] = true
	}
	return nil
}

// validChannel reports whether ch is one of the three channels §5 names. The channel
// set is fixed, so anything else is a validation failure rather than a lookup.
func validChannel(ch string) bool {
	return ch == "dev" || ch == "staging" || ch == "live"
}

// channelSource maps a channel to the channel directly below it on the dev -> staging
// -> live ladder. dev has no source, so promotion into it is invalid.
var channelSource = map[string]string{"staging": "dev", "live": "staging"}

// releaseHistoryItem is one row of a channel's release history.
type releaseHistoryItem struct {
	ReleaseID            int64     `json:"release_id"`
	ManifestSHA256       string    `json:"manifest_sha256"`
	ServerManifestSHA256 *string   `json:"server_manifest_sha256"`
	MinClientVersion     string    `json:"min_client_version"`
	Message              string    `json:"message"`
	CreatedBy            string    `json:"created_by"`
	CreatedAt            time.Time `json:"created_at"`
	IsHead               bool      `json:"is_head"`
}

// releaseHistoryResponse wraps a page of a channel's releases. HeadReleaseID is stated
// once so a client can mark the head without scanning the page.
type releaseHistoryResponse struct {
	HeadReleaseID int64                `json:"head_release_id"`
	Releases      []releaseHistoryItem `json:"releases"`
	NextBefore    *int64               `json:"next_before"`
}

// listReleases serves GET /channels/{ch}/releases.
func (h *Handlers) listReleases(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	if !validChannel(ch) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "channel must be one of dev, staging or live")
		return
	}

	before, ok := parseReleaseBefore(r.URL.Query().Get("before"))
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

	head, releases, next, err := h.Store.ListReleases(r.Context(), ch, before, limit)
	if err != nil {
		if errors.Is(err, store.ErrChannelNotFound) {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "no such channel: "+ch)
			return
		}
		h.internalError(w, r, err)
		return
	}

	items := make([]releaseHistoryItem, 0, len(releases))
	for _, rel := range releases {
		items = append(items, releaseHistoryItem{
			ReleaseID:            rel.ReleaseID,
			ManifestSHA256:       rel.ManifestSHA256,
			ServerManifestSHA256: optionalSHA(rel.ServerManifestSHA256),
			MinClientVersion:     rel.MinClientVersion,
			Message:              rel.Message,
			CreatedBy:            rel.CreatedBy,
			CreatedAt:            rel.CreatedAt,
			IsHead:               rel.IsHead,
		})
	}
	writeJSON(w, http.StatusOK, releaseHistoryResponse{
		HeadReleaseID: head,
		Releases:      items,
		NextBefore:    next,
	})
}

// rollbackRequest is the POST body. Both ids are pointers so an absent field is
// distinguishable from an explicit 0.
type rollbackRequest struct {
	ReleaseID     *int64 `json:"release_id"`
	BaseReleaseID *int64 `json:"base_release_id"`
}

// rollbackRelease serves POST /channels/{ch}/rollback. Auth has already established the
// caller is an admin.
func (h *Handlers) rollbackRelease(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	if !validChannel(ch) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "channel must be one of dev, staging or live")
		return
	}

	var req rollbackRequest
	if !readStrictBody(w, r, &req, maxReleaseBody) {
		return
	}
	if ve := validateRollback(req); ve != nil {
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

	rel, err := h.Store.Rollback(r.Context(), ch, *req.ReleaseID, *req.BaseReleaseID, actor)
	if err != nil {
		var stale *store.StaleReleaseError
		var notIn *store.ReleaseNotInChannelError
		switch {
		case errors.As(err, &stale):
			WriteError(w, r, http.StatusConflict, "stale_release",
				fmt.Sprintf("channel %s has moved from release %d to %d", stale.Channel, stale.Base, stale.Current))
			return
		case errors.As(err, &notIn):
			WriteError(w, r, http.StatusNotFound, "not_found",
				fmt.Sprintf("release %d is not a release of channel %s", notIn.ReleaseID, notIn.Channel))
			return
		case errors.Is(err, store.ErrNoReleaseChanges):
			WriteError(w, r, http.StatusConflict, "no_changes", "the channel head is already that release")
			return
		case errors.Is(err, store.ErrChannelNotFound):
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "no such channel: "+ch)
			return
		}
		h.internalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, releaseResponseFrom(rel))
}

// validateRollback applies the two field rules in a fixed order.
func validateRollback(req rollbackRequest) *validationError {
	switch {
	case req.ReleaseID == nil:
		return &validationError{"release_id", "release_id is required"}
	case *req.ReleaseID < 1:
		return &validationError{"release_id", "release_id must be at least 1"}
	case req.BaseReleaseID == nil:
		return &validationError{"base_release_id", "base_release_id is required"}
	case *req.BaseReleaseID < 1:
		return &validationError{"base_release_id", "base_release_id must be at least 1"}
	}
	return nil
}

// promoteRequest is the POST body. base_release_id is required; message is optional and
// defaults to a description naming the source release.
type promoteRequest struct {
	BaseReleaseID *int64 `json:"base_release_id"`
	Message       string `json:"message"`
}

// promoteRelease serves POST /channels/{ch}/promote?from=<channel>. Auth has already
// established the caller is an admin.
func (h *Handlers) promoteRelease(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	if !validChannel(ch) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "channel must be one of dev, staging or live")
		return
	}
	source, ok := channelSource[ch]
	if !ok {
		WriteError(w, r, http.StatusBadRequest, "validation_failed", "channel "+ch+" has no lower channel to promote from")
		return
	}
	from := r.URL.Query().Get("from")
	if from != source {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("from must be %s when promoting to %s", source, ch))
		return
	}

	var req promoteRequest
	if !readStrictBody(w, r, &req, maxReleaseBody) {
		return
	}
	if ve := validatePromote(req); ve != nil {
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

	message := req.Message
	if strings.TrimSpace(message) == "" {
		message = ""
	}

	rel, err := h.Store.Promote(r.Context(), ch, from, *req.BaseReleaseID, message, actor)
	if err != nil {
		var stale *store.StaleReleaseError
		switch {
		case errors.As(err, &stale):
			WriteError(w, r, http.StatusConflict, "stale_release",
				fmt.Sprintf("channel %s has moved from release %d to %d", stale.Channel, stale.Base, stale.Current))
			return
		case errors.Is(err, store.ErrNoReleaseChanges):
			WriteError(w, r, http.StatusConflict, "no_changes",
				fmt.Sprintf("channel %s already carries the content of %s", ch, from))
			return
		case errors.Is(err, store.ErrChannelNotFound):
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "no such channel: "+from)
			return
		}
		h.internalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, releaseResponseFrom(rel))
}

// validatePromote applies the field rules in a fixed order.
func validatePromote(req promoteRequest) *validationError {
	switch {
	case req.BaseReleaseID == nil:
		return &validationError{"base_release_id", "base_release_id is required"}
	case *req.BaseReleaseID < 1:
		return &validationError{"base_release_id", "base_release_id must be at least 1"}
	case strings.TrimSpace(req.Message) != "" && utf8.RuneCountInString(req.Message) > maxPublishMessage:
		return &validationError{"message", fmt.Sprintf("message must be at most %d characters", maxPublishMessage)}
	}
	return nil
}

// parseReleaseBefore reads the optional before cursor, a release id. An absent or empty
// value is no cursor; anything that is not a positive integer is a validation error.
func parseReleaseBefore(raw string) (*int64, bool) {
	if raw == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return nil, false
	}
	return &n, true
}
