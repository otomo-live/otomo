package authn

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MicahParks/keyfunc/v3"

	"gateway/gateway/internal/router"
)

// Defaults for the JWKS clients. None of these are env-tunable: the techspec's
// config table (§3) fixes the surface at the JWKS URL and the expected
// issuer/audience, and a knob nobody has needed yet is a knob nobody has tested.
const (
	// fetchTimeout bounds one JWKS request. Without it the library's own default
	// is a minute, which is long enough for a half-open connection to stall startup.
	fetchTimeout = 5 * time.Second

	// refreshInterval is how often each JWKS is re-fetched, and simultaneously the
	// retry interval when a fetch has failed.
	//
	// keyfunc's own default is one hour, which is the wrong shape here: an Auth
	// that is not yet listening when Gateway starts would leave /readyz at 503 for
	// up to an hour. 30s keeps that window operationally irrelevant while costing
	// one small GET per domain per interval.
	refreshInterval = 30 * time.Second
)

// Issuer is the identity a route Group's tokens are expected to carry. Both
// values are checked against the token's own claims, independently of which
// JWKS answered — a key being present in the player JWKS says nothing about who
// the token claims to have been issued by (techspec §6.3).
type Issuer struct {
	Issuer   string
	Audience string
}

// Registry owns one JWKS client per authentication domain.
//
// A domain's client is built once and lives for the process. That is deliberate:
// keyfunc starts its background refresh goroutine before performing the first
// fetch, so constructing a client per retry would leave one goroutine behind for
// every failed attempt. Instead each client is built once, the library's ticker
// is the retry, and readiness is derived from whether any key has arrived yet.
type Registry struct {
	sources map[router.Group]*source
}

// source is one JWKS endpoint's client. kf is nil until the first fetch
// completes; a nil client rejects everything, which is the correct direction to
// be wrong in for a process that has not yet seen any public key.
type source struct {
	group router.Group
	url   string
	kf    atomic.Pointer[keyfunc.Keyfunc]
}

// NewRegistry builds a registry with one source per entry in urls, keyed by the
// route Group that authenticates against it.
func NewRegistry(urls map[router.Group]string) *Registry {
	r := &Registry{sources: make(map[router.Group]*source, len(urls))}
	for group, u := range urls {
		r.sources[group] = &source{group: group, url: u}
	}
	return r
}

// Start begins fetching each domain's keys. It returns immediately: the
// listeners must be serving public routes whether or not Auth is up yet, and an
// unavailable JWKS endpoint must never keep the process from starting
// (06-auth-identity-contract.md §3).
//
// The passed context ends the clients' background refresh. Cancelling it does
// not close the listeners and is only expected at shutdown or in tests.
func (r *Registry) Start(ctx context.Context) {
	for _, s := range r.sources {
		go s.start(ctx)
	}
}

func (s *source) start(ctx context.Context) {
	kf, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{s.url}, keyfunc.Override{
		HTTPTimeout: fetchTimeout,
		// NoErrorReturnFirstHTTPReq stays at its default of true, so an
		// unreachable endpoint yields a working-but-empty client rather than an
		// error. keyfunc's ticker then keeps retrying until the service appears.
		RefreshInterval: refreshInterval,
		RefreshErrorHandlerFunc: func(string) func(context.Context, error) {
			return func(ctx context.Context, err error) {
				// Warn, not Error: until Auth is deployed this fires every
				// refreshInterval on a healthy gateway, and Error here would be
				// noise an operator learns to ignore. /readyz is the signal that
				// says whether it actually matters.
				slog.WarnContext(ctx, "jwks refresh failed",
					"group", s.group.String(), "url", s.url, "err", err)
			}
		},
	})
	if err != nil {
		// Only reachable for a malformed URL or an unparseable key set, not for
		// an unavailable host. This domain can never authenticate anyone; the
		// process keeps serving, and /readyz stays 503.
		slog.ErrorContext(ctx, "cannot build the jwks client; this domain will reject every token",
			"group", s.group.String(), "url", s.url, "err", err)
		return
	}
	s.kf.Store(&kf)
	slog.InfoContext(ctx, "jwks client started", "group", s.group.String(), "url", s.url)
}

// Keyfunc returns the client for a domain, or nil before its first successful
// fetch. Callers treat nil as "cannot verify, reject".
func (r *Registry) Keyfunc(group router.Group) keyfunc.Keyfunc {
	s, ok := r.sources[group]
	if !ok {
		return nil
	}
	if kf := s.kf.Load(); kf != nil {
		return *kf
	}
	return nil
}

// Ready reports whether every configured domain has at least one key. It is the
// whole of /readyz's JWKS half (techspec §4).
//
// A domain that has never been fetched from, or whose client failed to build,
// reports not-ready. A domain whose last refresh failed keeps its previous keys
// and stays ready — keyfunc only replaces the set on a successful fetch, which
// is what lets Gateway keep verifying tokens while Auth is briefly down.
func (r *Registry) Ready(ctx context.Context) bool {
	for _, s := range r.sources {
		if !s.ready(ctx) {
			return false
		}
	}
	return true
}

func (s *source) ready(ctx context.Context) bool {
	kf := s.kf.Load()
	if kf == nil {
		return false
	}
	keys, err := (*kf).Storage().KeyReadAll(ctx)
	return err == nil && len(keys) > 0
}

// Status reports, per domain, whether it is ready. It exists so a 503 can name
// the domain holding it rather than leaving an operator to guess.
func (r *Registry) Status(ctx context.Context) map[string]bool {
	out := make(map[string]bool, len(r.sources))
	for group, s := range r.sources {
		out[group.String()] = s.ready(ctx)
	}
	return out
}
