// Handlers is the seam between the route table and the code behind each route.
//
// Routes() states which paths exist and the least role that may call them; Handlers
// states which of them are implemented. A route with no entry here stays behind
// NotImplemented, so the 501 placeholders shrink one ticket at a time without the
// route table — or the role boundaries it encodes — moving.

package api

import (
	"log/slog"
	"net/http"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// Handlers carries everything a route handler needs from outside this package. Store
// is the Postgres access layer and Log is the process logger; both may be nil in a
// server built with a zero-value Deps, and For still answers every route — with
// NotImplemented, so a half-wired server fails closed rather than panicking.
//
// Schemas is the compiled-validator cache. A nil *schema.Cache is valid: it compiles on
// every call instead of caching, which keeps a test or a half-wired server correct.
//
// Blobs is the content-addressed store a version's canonical bytes are written to. It
// may be nil, in which case cutting a version fails closed with a 500.
type Handlers struct {
	Store   *store.DB
	Log     *slog.Logger
	Schemas *schema.Cache
	Blobs   *blob.Store

	// Packs is the subset of Store the content-pack handlers use. It exists so a
	// handler test can supply a fakeStore and exercise the streaming path without a
	// database; production leaves it nil and packStore() falls back to Store.
	Packs PackStore

	// MaxPackBytes caps a pack upload body. Zero means the built-in default, which
	// mirrors CONFIG_MAX_PACK_BYTES's own default.
	MaxPackBytes int64

	// PackUpload, when set, is called once per successful pack upload with the number
	// of bytes read. It is the seam that lets the server layer add them to its
	// Prometheus counter without this package importing the registry.
	PackUpload func(bytes int64)

	// Published, when set, is called once per successful release publish with the
	// channel published to. Rollback and promote are not publishes and never call it.
	Published func(channel string)

	// ValidationFailed, when set, is called once each time a document is found invalid
	// against its namespace schema. Malformed requests are not validation failures and
	// never call it.
	ValidationFailed func()
}

// For returns the handler for rt's pattern, or NotImplemented when the pattern has no
// implementation yet. A nil *Handlers is valid and returns NotImplemented for every
// route, which is what lets a test build Deps without any handlers and keep asserting
// the pre-implementation 501.
func (h *Handlers) For(rt Route) http.Handler {
	if h == nil {
		return http.HandlerFunc(NotImplemented)
	}
	if handler, ok := h.implemented()[rt.Pattern()]; ok {
		return handler
	}
	return http.HandlerFunc(NotImplemented)
}

// implemented is the map from ServeMux pattern to handler. For is called once per
// route while the mux is assembled, not per request, so building the map on each call
// costs nothing that matters.
func (h *Handlers) implemented() map[string]http.Handler {
	return map[string]http.Handler{
		http.MethodGet + " " + namespacesPath:  http.HandlerFunc(h.listNamespaces),
		http.MethodPost + " " + namespacesPath: http.HandlerFunc(h.createNamespace),

		http.MethodGet + " " + schemasPath: http.HandlerFunc(h.getSchema),
		http.MethodPut + " " + schemasPath: http.HandlerFunc(h.replaceSchema),

		http.MethodGet + " " + draftsPath: http.HandlerFunc(h.getDraft),
		http.MethodPut + " " + draftsPath: http.HandlerFunc(h.saveDraft),

		http.MethodPost + " " + draftValidatePath: http.HandlerFunc(h.validateDraft),

		http.MethodPost + " " + versionsPath: http.HandlerFunc(h.createVersion),
		http.MethodGet + " " + versionsPath:  http.HandlerFunc(h.listVersions),
		http.MethodGet + " " + versionPath:   http.HandlerFunc(h.getVersion),
		http.MethodGet + " " + diffPath:      http.HandlerFunc(h.diffVersions),

		http.MethodPost + " " + packsPath: http.HandlerFunc(h.createPack),
		http.MethodGet + " " + packsPath:  http.HandlerFunc(h.listPacks),

		http.MethodPost + " " + releasesPath:     http.HandlerFunc(h.publishRelease),
		http.MethodPost + " " + liveReleasesPath: http.HandlerFunc(h.publishRelease),
		http.MethodGet + " " + releasesPath:      http.HandlerFunc(h.listReleases),
		http.MethodPost + " " + rollbackPath:     http.HandlerFunc(h.rollbackRelease),
		http.MethodPost + " " + promotePath:      http.HandlerFunc(h.promoteRelease),

		http.MethodGet + " " + auditPath: http.HandlerFunc(h.listAudit),
	}
}
