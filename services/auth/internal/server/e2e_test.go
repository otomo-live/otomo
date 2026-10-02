package server_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/auth/internal/server"
	"github.com/otomo-live/otomo/services/auth/internal/testdb"
	"github.com/otomo-live/otomo/services/auth/internal/token"
)

// e2e is a real server on a real database, wired the way auth.go wires it: a signing
// key registered in signing_key, the JWKS built from the active rows, and the store
// behind every /auth route.
type e2e struct {
	h  *harness
	kf keyfunc.Keyfunc
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	db, ctx := testdb.Open(t)

	pub, priv, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	kid := "e2e-" + uuid.New().String()
	if err := db.InsertSigningKey(ctx, kid, pub); err != nil {
		t.Fatalf("InsertSigningKey: %v", err)
	}
	active, err := db.ListActiveSigningKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]token.PublicKey, 0, len(active))
	for _, k := range active {
		keys = append(keys, token.PublicKey{Kid: k.Kid, Key: ed25519.PublicKey(k.PublicKey)})
	}
	raw, err := token.BuildJWKS(keys)
	if err != nil {
		t.Fatal(err)
	}
	jwks := &token.JWKSCache{}
	jwks.Swap(raw)

	h := newHarness(t, server.Deps{
		JWKS: jwks,
		Signer: &token.Signer{
			Kid: kid, PrivateKey: priv,
			Issuer: "https://auth.otomo.internal", Audience: "otomo:player",
			TTL: 15 * time.Minute,
		},
		Accounts: db,
		Refresh:  db,
		Ready:    db.Ready,
	})

	// Gateway's real JWKS client, pointed at the JWKS this server publishes.
	kctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kf, err := keyfunc.NewDefaultCtx(kctx, []string{h.public + "/.well-known/jwks.json"})
	if err != nil {
		t.Fatalf("keyfunc: %v", err)
	}
	return &e2e{h: h, kf: kf}
}

type session struct {
	SchemaVersion int    `json:"schema_version"`
	AccessToken   string `json:"access_token"`
	ExpiresIn     int    `json:"expires_in"`
	RefreshToken  string `json:"refresh_token"`
}

// call POSTs body to path and returns the status, the parsed error code (if any) and
// the raw body.
func (e *e2e) call(t *testing.T, path, body string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Post(e.h.public+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 && resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("POST %s sent WWW-Authenticate", path)
	}
	return resp.StatusCode, env.Error.Code, raw
}

// ok expects a 200 carrying a full session and returns it with its verified subject.
func (e *e2e) ok(t *testing.T, path, body string) (session, string) {
	t.Helper()
	status, code, raw := e.call(t, path, body)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d %s %s, want 200", path, status, code, raw)
	}
	var s session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != 1 || s.ExpiresIn != 900 || s.RefreshToken == "" ||
		strings.Contains(string(raw), `"services"`) {
		t.Errorf("POST %s body = %s", path, raw)
	}
	return s, e.verify(t, s.AccessToken)
}

// verify checks an access token exactly as Gateway's authn middleware and Session's
// player verifier do, and returns its subject.
func (e *e2e) verify(t *testing.T, access string) string {
	t.Helper()
	claims := &token.Claims{}
	_, err := jwt.ParseWithClaims(access, claims, e.kf.KeyfuncCtx(context.Background()),
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer("https://auth.otomo.internal"),
		jwt.WithAudience("otomo:player"),
		jwt.WithLeeway(30*time.Second),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		t.Fatalf("access token rejected by Gateway's parser: %v", err)
	}
	if _, err := uuid.Parse(claims.Subject); err != nil {
		t.Fatalf("sub %q would be refused by Session: %v", claims.Subject, err)
	}
	if len(claims.Roles) != 0 {
		t.Errorf("player token carries roles %v", claims.Roles)
	}
	return claims.Subject
}

func (e *e2e) expect(t *testing.T, path, body string, wantStatus int, wantCode string) {
	t.Helper()
	status, code, raw := e.call(t, path, body)
	if status != wantStatus || code != wantCode {
		t.Errorf("POST %s %s = %d %q (%s), want %d %q", path, body, status, code, raw, wantStatus, wantCode)
	}
}

func refreshBody(tok string) string { return `{"refresh_token":"` + tok + `"}` }

// TestPlayerLoginEndToEnd plays the whole player-session contract against Postgres:
// login, stable sub, refresh chain, reuse revocation, logout, and the error codes.
func TestPlayerLoginEndToEnd(t *testing.T) {
	e := newE2E(t)
	device := strings.Repeat("e", 11) + strings.ReplaceAll(uuid.New().String(), "-", "")
	other := strings.Repeat("o", 11) + strings.ReplaceAll(uuid.New().String(), "-", "")

	// Same device, same sub; another device, another sub.
	first, sub := e.ok(t, "/auth/anonymous", `{"device_id":"`+device+`"}`)
	second, again := e.ok(t, "/auth/anonymous", `{"device_id":"`+device+`"}`)
	if again != sub {
		t.Fatalf("same device_id gave subs %s and %s", sub, again)
	}
	if _, otherSub := e.ok(t, "/auth/anonymous", `{"device_id":"`+other+`"}`); otherSub == sub {
		t.Fatal("two device_ids share a sub")
	}

	// Refresh keeps the sub and rotates the refresh token.
	rotated, refreshedSub := e.ok(t, "/auth/refresh", refreshBody(first.RefreshToken))
	if refreshedSub != sub || rotated.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh gave sub %s token %q", refreshedSub, rotated.RefreshToken)
	}

	// Replaying the spent token is reuse: 401, and the whole family dies.
	e.expect(t, "/auth/refresh", refreshBody(first.RefreshToken), http.StatusUnauthorized, "invalid_token")
	e.expect(t, "/auth/refresh", refreshBody(rotated.RefreshToken), http.StatusUnauthorized, "invalid_token")

	// The second login is its own family and survives the first family's revocation.
	chained, _ := e.ok(t, "/auth/refresh", refreshBody(second.RefreshToken))

	// Logout revokes that family and always answers 204.
	e.expect(t, "/auth/logout", refreshBody(chained.RefreshToken), http.StatusNoContent, "")
	e.expect(t, "/auth/refresh", refreshBody(chained.RefreshToken), http.StatusUnauthorized, "invalid_token")
	e.expect(t, "/auth/logout", refreshBody(chained.RefreshToken), http.StatusNoContent, "")
	e.expect(t, "/auth/logout", `not json`, http.StatusNoContent, "")

	// Logging in again after logout is how a client recovers: same device, same sub.
	if _, back := e.ok(t, "/auth/anonymous", `{"device_id":"`+device+`"}`); back != sub {
		t.Errorf("re-login after logout gave sub %s, want %s", back, sub)
	}

	// Error codes.
	e.expect(t, "/auth/anonymous", `{"device_id":"short"}`, http.StatusBadRequest, "validation_failed")
	e.expect(t, "/auth/anonymous", `{`, http.StatusBadRequest, "validation_failed")
	e.expect(t, "/auth/refresh", `{`, http.StatusBadRequest, "validation_failed")
	e.expect(t, "/auth/refresh", `{}`, http.StatusUnauthorized, "invalid_token")
	e.expect(t, "/auth/refresh", refreshBody(strings.Repeat("A", 43)), http.StatusUnauthorized, "invalid_token")

	for _, path := range []string{"/auth/anonymous", "/auth/refresh", "/auth/logout"} {
		status, header, _ := get(t, e.h.public+path)
		if status != http.StatusMethodNotAllowed || header.Get("Allow") != http.MethodPost {
			t.Errorf("GET %s = %d Allow %q, want 405 Allow: POST", path, status, header.Get("Allow"))
		}
	}
}

// TestJWKSServesTheKeyThatSignsTokens checks the published document is what a
// verifier needs: no-store, and the signing kid present.
func TestJWKSServesTheKeyThatSignsTokens(t *testing.T) {
	e := newE2E(t)
	status, header, body := get(t, e.h.public+"/.well-known/jwks.json")
	if status != http.StatusOK || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("JWKS = %d Cache-Control %q", status, header.Get("Cache-Control"))
	}
	s, _ := e.ok(t, "/auth/anonymous", `{"device_id":"`+strings.Repeat("j", 43)+`"}`)
	parsed, _, err := jwt.NewParser().ParseUnverified(s.AccessToken, &token.Claims{})
	if err != nil {
		t.Fatal(err)
	}
	if kid, _ := parsed.Header["kid"].(string); kid == "" || !strings.Contains(body, `"kid":"`+kid+`"`) {
		t.Errorf("token kid %v is not in the served JWKS", parsed.Header["kid"])
	}
}
