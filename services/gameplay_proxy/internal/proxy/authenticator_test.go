package proxy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/ticket"
)

// fakeClock is a manually advanced clock, used so a test can cross the JWKS refresh
// interval without sleeping.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock(at time.Time) *fakeClock {
	return &fakeClock{at: at}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// jwksServer is a mutable JWKS endpoint: a test can publish a new key set between
// verifications to model a rotation, and count fetches to check the rate limit.
type jwksServer struct {
	server *httptest.Server
	mu     sync.Mutex
	doc    []byte
	hits   int
}

func newJWKSServer(t *testing.T) *jwksServer {
	t.Helper()
	k := &jwksServer{}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		k.mu.Lock()
		doc := k.doc
		k.hits++
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(k.server.Close)
	return k
}

// set replaces the published key set. Marshalling happens on the test goroutine so a
// failure is reported there rather than from the HTTP handler's goroutine.
func (k *jwksServer) set(t *testing.T, pubs ...ed25519.PublicKey) {
	t.Helper()
	set := ticket.JWKSet{}
	for _, pub := range pubs {
		set.Keys = append(set.Keys, ticket.JWK{
			Kty: "OKP",
			Crv: "Ed25519",
			Kid: ticket.Thumbprint(pub),
			X:   base64.RawURLEncoding.EncodeToString(pub),
			Use: "sig",
			Alg: "EdDSA",
		})
	}
	doc, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	k.mu.Lock()
	k.doc = doc
	k.mu.Unlock()
}

func (k *jwksServer) url() string          { return k.server.URL }
func (k *jwksServer) client() *http.Client { return k.server.Client() }

func (k *jwksServer) hitCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.hits
}

// testAuthenticator builds a loaded Authenticator against ks. The caller owns the key
// material and the clock.
func testAuthenticator(t *testing.T, ks *jwksServer, now func() time.Time) *Authenticator {
	t.Helper()
	a := NewAuthenticator(AuthenticatorConfig{
		JWKSURL:         ks.url(),
		Issuer:          testIssuer,
		Audience:        testAudience,
		Leeway:          TicketLeeway,
		RefreshInterval: 10 * time.Second,
		Client:          ks.client(),
		Now:             now,
		Logger:          discardLogger(),
	})
	if err := a.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return a
}

// TestAuthenticatorVerify drives the reference verifier through the proxy's wrapper.
func TestAuthenticatorVerify(t *testing.T) {
	pub, priv := mustKey(t)
	kid := ticket.Thumbprint(pub)
	ks := newJWKSServer(t)
	ks.set(t, pub)
	auth := testAuthenticator(t, ks, nil)

	validExp := time.Now().Add(time.Minute)
	cases := []struct {
		name    string
		ticket  string
		wantErr error
	}{
		{
			name:   "valid",
			ticket: issue(t, priv, kid, "gs-1", validExp),
		},
		{
			name:    "expired",
			ticket:  issue(t, priv, kid, "gs-1", time.Now().Add(-time.Minute)),
			wantErr: ticket.ErrExpired,
		},
		{
			name:    "wrong issuer",
			ticket:  signTicket(t, priv, kid, "https://evil.internal", testAudience, testPlayer, testAlloc, "gs-1", randomJTI(t), validExp, jwt.SigningMethodEdDSA),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "wrong audience",
			ticket:  signTicket(t, priv, kid, testIssuer, "otomo:dashboard", testPlayer, testAlloc, "gs-1", randomJTI(t), validExp, jwt.SigningMethodEdDSA),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "hs256",
			ticket:  signTicket(t, priv, kid, testIssuer, testAudience, testPlayer, testAlloc, "gs-1", randomJTI(t), validExp, jwt.SigningMethodHS256),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "none",
			ticket:  signTicket(t, priv, kid, testIssuer, testAudience, testPlayer, testAlloc, "gs-1", randomJTI(t), validExp, jwt.SigningMethodNone),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "missing srv",
			ticket:  signTicket(t, priv, kid, testIssuer, testAudience, testPlayer, testAlloc, "", randomJTI(t), validExp, jwt.SigningMethodEdDSA),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "missing jti",
			ticket:  signTicket(t, priv, kid, testIssuer, testAudience, testPlayer, testAlloc, "gs-1", "", validExp, jwt.SigningMethodEdDSA),
			wantErr: ticket.ErrInvalid,
		},
		{
			name:    "garbage",
			ticket:  "not-a-jwt",
			wantErr: ticket.ErrInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := auth.Verify(t.Context(), tc.ticket)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Verify: %v", err)
			case tc.wantErr == nil:
				if claims == nil || claims.Srv != "gs-1" {
					t.Fatalf("claims = %+v, want srv gs-1", claims)
				}
			case !errors.Is(err, tc.wantErr):
				t.Fatalf("Verify error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestAuthenticatorRefetchesUnknownKid models a rotation: the proxy loaded key A, the
// Allocator then publishes A and B, and a ticket signed with B triggers one refetch and
// then verifies. A later unknown kid is rate-limited until the interval passes.
func TestAuthenticatorRefetchesUnknownKid(t *testing.T) {
	pubA, _ := mustKey(t)
	pubB, privB := mustKey(t)
	pubC, privC := mustKey(t)

	ks := newJWKSServer(t)
	ks.set(t, pubA)

	clock := newFakeClock(time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC))
	auth := testAuthenticator(t, ks, clock.Now)
	if got := auth.keyCount(); got != 1 {
		t.Fatalf("key count after load = %d, want 1", got)
	}
	if hits := ks.hitCount(); hits != 1 {
		t.Fatalf("JWKS fetches after load = %d, want 1", hits)
	}

	// The new key is published but the proxy has not seen it yet.
	ks.set(t, pubA, pubB)
	ticketB := issue(t, privB, ticket.Thumbprint(pubB), "gs-1", clock.Now().Add(time.Hour))
	if _, err := auth.Verify(t.Context(), ticketB); err != nil {
		t.Fatalf("Verify with an unknown kid did not refetch: %v", err)
	}
	if got := auth.keyCount(); got != 2 {
		t.Errorf("key count after refetch = %d, want 2", got)
	}
	if hits := ks.hitCount(); hits != 2 {
		t.Errorf("JWKS fetches after refetch = %d, want 2", hits)
	}

	// A second unknown kid inside the interval may not fetch again.
	ks.set(t, pubA, pubB, pubC)
	ticketC := issue(t, privC, ticket.Thumbprint(pubC), "gs-1", clock.Now().Add(time.Hour))
	if _, err := auth.Verify(t.Context(), ticketC); !errors.Is(err, ticket.ErrUnknownKey) {
		t.Fatalf("rate-limited unknown kid error = %v, want ErrUnknownKey", err)
	}
	if hits := ks.hitCount(); hits != 2 {
		t.Errorf("JWKS fetches inside the interval = %d, want 2", hits)
	}

	// Once the interval has passed the same ticket is accepted.
	clock.Advance(11 * time.Second)
	ticketC = issue(t, privC, ticket.Thumbprint(pubC), "gs-1", clock.Now().Add(time.Hour))
	if _, err := auth.Verify(t.Context(), ticketC); err != nil {
		t.Fatalf("Verify after the interval: %v", err)
	}
	if hits := ks.hitCount(); hits != 3 {
		t.Errorf("JWKS fetches after the interval = %d, want 3", hits)
	}
}

// TestParseJWKSRejectsBadDocuments checks that a broken publishing endpoint is an
// error, not a silently empty key set.
func TestParseJWKSRejectsBadDocuments(t *testing.T) {
	pub, _ := mustKey(t)
	good := ticket.JWKS(pub)

	cases := map[string][]byte{
		"no keys":    []byte(`{"keys":[]}`),
		"bad kty":    []byte(`{"keys":[{"kty":"RSA","crv":"Ed25519","kid":"x","x":"AAAA"}]}`),
		"no kid":     []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA"}]}`),
		"wrong size": []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"x","x":"AAAA"}]}`),
		"not json":   []byte(`{`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ticket.ParseJWKS(raw); err == nil {
				t.Fatalf("ParseJWKS accepted %s", name)
			}
		})
	}

	keys, err := ticket.ParseJWKS(good)
	if err != nil {
		t.Fatalf("ParseJWKS(good): %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("parsed %d keys, want 1", len(keys))
	}
}
