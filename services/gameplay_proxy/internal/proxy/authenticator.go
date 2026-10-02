// Ticket verification with JWKS refresh.
//
// The proxy verifies tickets exactly as services/allocator/internal/token does; this
// type adds the one thing the proxy needs on top of the reference verifier: when a
// ticket names a kid the current key set does not know, it refetches the Allocator's
// JWKS and tries once more. The refresh is rate-limited to once per
// refreshInterval so a stream of forged tickets cannot turn into a fetch flood
// (doc 14 §5 fixes the limit at 10 s).

package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/ticket"
)

// AuthenticatorConfig is everything an Authenticator needs. JWKSURL, Issuer and
// Audience have no defaults because a wrong one silently accepts or rejects every
// ticket; the caller must supply them from configuration.
type AuthenticatorConfig struct {
	JWKSURL         string
	Issuer          string
	Audience        string
	Leeway          time.Duration
	RefreshInterval time.Duration
	Client          *http.Client
	Now             func() time.Time
	Logger          *slog.Logger
}

// Authenticator owns the live key set and the rate-limited refetch. Build it with
// NewAuthenticator, load it with Load (or Run), and call Verify per handshake. It is
// safe for concurrent use.
type Authenticator struct {
	cfg    AuthenticatorConfig
	client *http.Client
	now    func() time.Time
	log    *slog.Logger

	keys *ticket.KeySet

	// gate serialises refreshes and guards lastAttempt, so two tickets naming the
	// same unknown kid produce at most one fetch per interval.
	gate        sync.Mutex
	lastAttempt time.Time
}

// NewAuthenticator returns an Authenticator with an empty key set. Loaded stays false
// until the first successful Load, which is what /readyz waits on.
func NewAuthenticator(cfg AuthenticatorConfig) *Authenticator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Authenticator{
		cfg:    cfg,
		client: client,
		now:    cfg.Now,
		log:    log,
		keys:   ticket.NewKeySet(),
	}
}

// Load fetches the JWKS once and installs it, reporting any error. It is unconditional:
// callers that need the rate limit use Verify, which goes through refresh.
func (a *Authenticator) Load(ctx context.Context) error {
	keys, err := ticket.FetchJWKS(ctx, a.client, a.cfg.JWKSURL)
	if err != nil {
		return err
	}
	a.keys.Replace(keys)
	return nil
}

// Run keeps trying Load until it succeeds or ctx is cancelled, so a proxy that starts
// before the Allocator is up still becomes ready without a restart. Once loaded it
// returns; the next refresh is driven by an unknown kid, not by a timer, because the
// contract only requires a refetch when a kid is unknown.
func (a *Authenticator) Run(ctx context.Context, retry time.Duration) {
	for {
		if err := a.Load(ctx); err == nil {
			a.log.Info("ticket JWKS loaded", slog.String("url", a.cfg.JWKSURL))
			return
		} else {
			a.log.Warn("ticket JWKS load failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// Loaded reports whether a key set has ever been installed.
func (a *Authenticator) Loaded() bool {
	return a.keys.Loaded()
}

// Verify checks one ticket, refreshing the key set once if the kid is unknown. Every
// non-nil error wraps ticket.ErrInvalid, ticket.ErrExpired or ticket.ErrUnknownKey, so
// the caller switches on the sentinel rather than parsing a message.
func (a *Authenticator) Verify(ctx context.Context, raw string) (*ticket.Claims, error) {
	claims, err := a.verifyWithCurrentKeys(raw)
	if errors.Is(err, ticket.ErrUnknownKey) && a.refresh(ctx) {
		// A refresh was attempted. Re-verify even if the fetch failed: on failure the
		// call is cheap and the returned unknown-key error is still accurate.
		claims, err = a.verifyWithCurrentKeys(raw)
	}
	return claims, err
}

// verifyWithCurrentKeys builds a verifier from a snapshot of the set and uses it. The
// snapshot is taken per call so a concurrent Replace cannot mutate the map a verifier
// is holding.
func (a *Authenticator) verifyWithCurrentKeys(raw string) (*ticket.Claims, error) {
	v := ticket.NewVerifier(a.keys.Snapshot(), a.cfg.Issuer, a.cfg.Audience, a.cfg.Leeway, a.now)
	return v.Verify(raw)
}

// refresh attempts a JWKS fetch if the last attempt is at least RefreshInterval ago and
// reports whether it tried. The attempt is recorded before the request, so a failing
// Allocator is not retried once per forged ticket.
func (a *Authenticator) refresh(ctx context.Context) bool {
	a.gate.Lock()
	defer a.gate.Unlock()

	if !a.lastAttempt.IsZero() && a.now().Sub(a.lastAttempt) < a.cfg.RefreshInterval {
		return false
	}
	a.lastAttempt = a.now()

	if err := a.Load(ctx); err != nil {
		a.log.Warn("ticket JWKS refresh failed", slog.Any("error", err))
	}
	return true
}

// keyCount returns how many keys are currently installed. It exists for tests and for
// an operator log line, and it deliberately exposes no key material.
func (a *Authenticator) keyCount() int {
	return len(a.keys.Snapshot())
}
