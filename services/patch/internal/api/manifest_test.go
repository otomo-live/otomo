package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/patch/internal/auth"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
)

const (
	testKid      = "test-staff-key"
	testIssuer   = "https://php-admin.otomo.internal"
	testAudience = "otomo:staff"
)

// newTestVerifier returns a started verifier backed by a fresh in-process JWKS,
// waited until it has fetched the key so a rejection test means "this token", not
// "the key set was still empty".
func newTestVerifier(t *testing.T) (*auth.Verifier, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": testKid,
		"x":   base64.RawURLEncoding.EncodeToString(pub),
		"alg": "EdDSA",
		"use": "sig",
	}}}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	v := auth.NewVerifier(srv.URL, testIssuer, testAudience, time.Minute, 0)
	v.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for !v.Ready(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("verifier never fetched the jwks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return v, priv
}

// signTestToken mints a staff token. mut lets a test change exactly one claim.
func signTestToken(t *testing.T, priv ed25519.PrivateKey, mut func(jwt.MapClaims)) string {
	t.Helper()

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "staff-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"roles": []string{"viewer"},
	}
	if mut != nil {
		mut(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// observer is a manifest observer that records instead of counting, so a test can
// assert which results were recorded without scraping a registry.
type observer struct {
	requests []string
	rejected []string
}

func (o *observer) ManifestRequest(channel, result string) {
	o.requests = append(o.requests, channel+"/"+result)
}

func (o *observer) TokenRejected(reason string) {
	o.rejected = append(o.rejected, reason)
}

func holderWith(entries map[string]*manifest.Entry) *manifest.Holder {
	return manifest.NewHolder(manifest.NewSet(entries))
}

func serve(t *testing.T, h http.HandlerFunc, method, channel, ifNoneMatch, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "/patch/v1/"+channel+"/manifest", nil)
	req.SetPathValue("channel", channel)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the COM-5 envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Message == "" {
		t.Error("the error envelope has no message")
	}
	return env.Error.Code
}

// TestIfNoneMatch pins the RFC 9110 §13.1.2 list handling, including the two weak
// comparisons and the decision to skip a malformed member rather than reject the
// request.
func TestIfNoneMatch(t *testing.T) {
	tests := []struct {
		name   string
		header string
		etag   string
		want   bool
	}{
		{"empty header", "", `"abc123"`, false},
		{"exact", `"abc123"`, `"abc123"`, true},
		{"list", `"other", "abc123"`, `"abc123"`, true},
		{"spaces around comma", `"other" ,  "abc123"`, `"abc123"`, true},
		{"whitespace around member", "  \"abc123\"  ", `"abc123"`, true},
		{"weak header", `W/"abc123"`, `"abc123"`, true},
		{"weak current", `"abc123"`, `W/"abc123"`, true},
		{"both weak", `W/"abc123"`, `W/"abc123"`, true},
		{"star", "*", `"abc123"`, true},
		{"no match", `"nope"`, `"abc123"`, false},
		{"malformed members ignored", `garbage, "abc123"`, `"abc123"`, true},
		{"all malformed", `garbage, "unterminated`, `"abc123"`, false},
		{"bare opaque tag", `abc123`, `"abc123"`, false},
		{"trailing comma", `"abc123",`, `"abc123"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("If-None-Match", tt.header)
			}
			if got := ifNoneMatch(req, tt.etag); got != tt.want {
				t.Errorf("ifNoneMatch(%q, %q) = %v, want %v", tt.header, tt.etag, got, tt.want)
			}
		})
	}
}

// TestManifestLiveServesBytesAndValidators covers the public 200: the body is the
// pre-serialized bytes verbatim and all three caching headers are present.
func TestManifestLiveServesBytesAndValidators(t *testing.T) {
	body := []byte(`{"release":7,"packs":[]}`)
	obs := &observer{}
	h := Manifest(holderWith(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: body, ETag: `"deadbeef"`, MinClientVersion: "1.2.3"},
	}), nil, obs)

	rec := serve(t, h, http.MethodGet, "live", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), body)
	}
	if got := rec.Header().Get("ETag"); got != `"deadbeef"` {
		t.Errorf("ETag = %q, want %q", got, `"deadbeef"`)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("X-Min-Client-Version"); got != "1.2.3" {
		t.Errorf("X-Min-Client-Version = %q, want 1.2.3", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if len(obs.requests) != 1 || obs.requests[0] != "live/200" {
		t.Errorf("recorded requests = %v, want [live/200]", obs.requests)
	}
}

// TestManifestLiveRevalidation covers the 304: empty body, the same validators, no
// content type, and a 304 recorded separately from the 200.
func TestManifestLiveRevalidation(t *testing.T) {
	obs := &observer{}
	h := Manifest(holderWith(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: []byte(`{"x":1}`), ETag: `"etag-1"`, MinClientVersion: "0.9.0"},
	}), nil, obs)

	rec := serve(t, h, http.MethodGet, "live", `W/"etag-1"`, "")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"etag-1"` {
		t.Errorf("ETag = %q, want %q", got, `"etag-1"`)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("X-Min-Client-Version"); got != "0.9.0" {
		t.Errorf("X-Min-Client-Version = %q, want 0.9.0", got)
	}
	if len(obs.requests) != 1 || obs.requests[0] != "live/304" {
		t.Errorf("recorded requests = %v, want [live/304]", obs.requests)
	}
}

// TestManifestHeadServesNoBody checks that a HEAD gets the 200 and the validators
// but no manifest bytes, while still counting as a served manifest.
func TestManifestHeadServesNoBody(t *testing.T) {
	obs := &observer{}
	h := Manifest(holderWith(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: []byte(`{"x":1}`), ETag: `"etag-1"`, MinClientVersion: "0.9.0"},
	}), nil, obs)

	rec := serve(t, h, http.MethodHead, "live", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"etag-1"` {
		t.Errorf("ETag = %q, want %q", got, `"etag-1"`)
	}
	if len(obs.requests) != 1 || obs.requests[0] != "live/200" {
		t.Errorf("recorded requests = %v, want [live/200]", obs.requests)
	}
}

// TestManifestErrors covers the four non-manifest outcomes the route can produce
// without a token: unknown channel, unloaded channel and a nil holder.
func TestManifestErrors(t *testing.T) {
	loaded := Manifest(holderWith(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: []byte(`{}`), ETag: `"e"`, MinClientVersion: "1"},
	}), nil, nil)

	t.Run("unknown channel", func(t *testing.T) {
		rec := serve(t, loaded, http.MethodGet, "qa", "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if got := errorCode(t, rec); got != "not_found" {
			t.Errorf("code = %q, want not_found", got)
		}
	})

	t.Run("channel not loaded", func(t *testing.T) {
		h := Manifest(holderWith(nil), nil, nil)
		rec := serve(t, h, http.MethodGet, "live", "", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if got := errorCode(t, rec); got != "not_ready" {
			t.Errorf("code = %q, want not_ready", got)
		}
	})

	t.Run("nil holder", func(t *testing.T) {
		h := Manifest(nil, nil, nil)
		rec := serve(t, h, http.MethodGet, "live", "", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if got := errorCode(t, rec); got != "not_ready" {
			t.Errorf("code = %q, want not_ready", got)
		}
	})
}

// TestManifestRestrictedChannelAuth is the dev/staging token table: no token, a player
// token, a valid staff token with no role, and a viewer.
func TestManifestRestrictedChannelAuth(t *testing.T) {
	verifier, priv := newTestVerifier(t)
	dev := manifest.NewSet(map[string]*manifest.Entry{
		"dev": {Channel: "dev", Body: []byte(`{"dev":true}`), ETag: `"dev-etag"`, MinClientVersion: "1.0.0"},
	})

	tests := []struct {
		name   string
		token  string
		status int
		code   string
		reason string
	}{
		{
			name:   "no token",
			status: http.StatusUnauthorized,
			code:   auth.ReasonMissingToken,
			reason: auth.ReasonMissingToken,
		},
		{
			name: "player token",
			token: signTestToken(t, priv, func(c jwt.MapClaims) {
				c["aud"] = "otomo:player"
			}),
			status: http.StatusUnauthorized,
			code:   auth.ReasonAudienceMismatch,
			reason: auth.ReasonAudienceMismatch,
		},
		{
			name: "player issuer",
			token: signTestToken(t, priv, func(c jwt.MapClaims) {
				c["iss"] = "https://auth.otomo.internal"
			}),
			status: http.StatusUnauthorized,
			code:   auth.ReasonIssuerMismatch,
			reason: auth.ReasonIssuerMismatch,
		},
		{
			name: "staff token without roles",
			token: signTestToken(t, priv, func(c jwt.MapClaims) {
				delete(c, "roles")
			}),
			status: http.StatusForbidden,
			code:   auth.ReasonInsufficientRole,
			reason: auth.ReasonInsufficientRole,
		},
		{
			name:   "viewer",
			token:  signTestToken(t, priv, nil),
			status: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &observer{}
			handler := Manifest(manifest.NewHolder(dev), verifier, obs)
			authHeader := ""
			if tt.token != "" {
				authHeader = "Bearer " + tt.token
			}
			rec := serve(t, handler, http.MethodGet, "dev", "", authHeader)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.status, rec.Body.String())
			}
			if tt.status == http.StatusOK {
				if !bytes.Equal(rec.Body.Bytes(), []byte(`{"dev":true}`)) {
					t.Errorf("body = %q", rec.Body.String())
				}
				if len(obs.rejected) != 0 {
					t.Errorf("rejections = %v, want none", obs.rejected)
				}
				return
			}
			if got := errorCode(t, rec); got != tt.code {
				t.Errorf("code = %q, want %q", got, tt.code)
			}
			if len(obs.rejected) != 1 || obs.rejected[0] != tt.reason {
				t.Errorf("recorded rejections = %v, want [%s]", obs.rejected, tt.reason)
			}
		})
	}
}
