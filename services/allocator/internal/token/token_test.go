package token_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/otomo-live/otomo/services/allocator/internal/token"
)

const (
	testIssuer   = "https://allocator.otomo.internal"
	testAudience = "otomo:gameserver"

	testPlayerID     = "3f1a2b4c-5d6e-4f70-8192-a3b4c5d6e7f8"
	testAllocationID = "9c8b7a65-4321-4f0e-9d8c-7b6a5c4d3e2f"
	testServerID     = "gameserver-01"

	testTTL = 5 * time.Minute
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func testIssuerFor(priv ed25519.PrivateKey, now func() time.Time) *token.Issuer {
	return token.NewIssuer(priv, testIssuer, testAudience, testTTL, now)
}

func testVerifier(kid string, pub ed25519.PublicKey, leeway time.Duration, now func() time.Time) *token.Verifier {
	return token.NewVerifier(map[string]ed25519.PublicKey{kid: pub}, testIssuer, testAudience, leeway, now)
}

// headerKid reads the unverified header so a test can assert on the kid without
// depending on the verifier succeeding.
func headerKid(t *testing.T, signed string) string {
	t.Helper()
	parsed, _, err := jwt.NewParser().ParseUnverified(signed, &token.Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	kid, _ := parsed.Header["kid"].(string)
	return kid
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)

	signed, expiresAt, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if got, want := issuer.Kid(), token.Thumbprint(pub); got != want {
		t.Errorf("Kid = %q, want RFC 7638 thumbprint %q", got, want)
	}
	if got := headerKid(t, signed); got != issuer.Kid() {
		t.Errorf("header kid = %q, want %q", got, issuer.Kid())
	}

	claims, err := testVerifier(issuer.Kid(), pub, 0, time.Now).Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != testPlayerID {
		t.Errorf("sub = %q, want %q", claims.Subject, testPlayerID)
	}
	if claims.Alloc != testAllocationID {
		t.Errorf("alloc = %q, want %q", claims.Alloc, testAllocationID)
	}
	if claims.Srv != testServerID {
		t.Errorf("srv = %q, want %q", claims.Srv, testServerID)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("iss = %q, want %q", claims.Issuer, testIssuer)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testAudience {
		t.Errorf("aud = %v, want [%s]", claims.Audience, testAudience)
	}
	if claims.ID == "" {
		t.Error("jti is empty")
	}
	if claims.IssuedAt == nil {
		t.Error("iat is missing")
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Unix() != expiresAt.Unix() {
		t.Errorf("exp = %v, want the returned %v", claims.ExpiresAt, expiresAt)
	}
}

func TestVerifyRejectsWrongIssuerAndAudience(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)
	signed, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	keys := map[string]ed25519.PublicKey{issuer.Kid(): pub}

	cases := map[string]*token.Verifier{
		"issuer":   token.NewVerifier(keys, "https://evil.internal", testAudience, 0, time.Now),
		"audience": token.NewVerifier(keys, testIssuer, "otomo:dashboard", 0, time.Now),
	}
	for name, verifier := range cases {
		if _, err := verifier.Verify(signed); !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%s mismatch: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestVerifyReportsExpired(t *testing.T) {
	base := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, fixedClock(base))

	signed, expiresAt, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if want := base.Add(testTTL); !expiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", expiresAt, want)
	}

	after := fixedClock(base.Add(testTTL + time.Second))
	if _, err := testVerifier(issuer.Kid(), pub, 0, after).Verify(signed); !errors.Is(err, token.ErrExpired) {
		t.Errorf("expired ticket: err = %v, want ErrExpired", err)
	}
}

func TestVerifyHonoursLeeway(t *testing.T) {
	base := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, fixedClock(base))
	signed, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	tenPast := fixedClock(base.Add(testTTL + 10*time.Second))
	if _, err := testVerifier(issuer.Kid(), pub, 0, tenPast).Verify(signed); !errors.Is(err, token.ErrExpired) {
		t.Errorf("no leeway: err = %v, want ErrExpired", err)
	}
	if _, err := testVerifier(issuer.Kid(), pub, 30*time.Second, tenPast).Verify(signed); err != nil {
		t.Errorf("10s past exp inside a 30s leeway: %v", err)
	}

	beyond := fixedClock(base.Add(testTTL + 31*time.Second))
	if _, err := testVerifier(issuer.Kid(), pub, 30*time.Second, beyond).Verify(signed); !errors.Is(err, token.ErrExpired) {
		t.Errorf("31s past exp beyond a 30s leeway: err = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)
	signed, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	raw["srv"] = "gameserver-99"
	tampered, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(tampered)

	if _, err := testVerifier(issuer.Kid(), pub, 0, time.Now).Verify(strings.Join(parts, ".")); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("tampered payload: err = %v, want ErrInvalid", err)
	}
}

func TestVerifyRejectsNoneAndHMAC(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)
	verifier := testVerifier(issuer.Kid(), pub, 0, time.Now)

	now := time.Now()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			Subject:   testPlayerID,
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        "fixed-jti",
		},
		Alloc: testAllocationID,
		Srv:   testServerID,
	}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	none.Header["kid"] = issuer.Kid()
	noneTicket, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg none: %v", err)
	}
	if _, err := verifier.Verify(noneTicket); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("alg none: err = %v, want ErrInvalid", err)
	}

	hmac := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hmac.Header["kid"] = issuer.Kid()
	hmacTicket, err := hmac.SignedString([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := verifier.Verify(hmacTicket); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("HS256: err = %v, want ErrInvalid", err)
	}
}

func TestVerifyRejectsUnknownKid(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)
	signed, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	verifier := token.NewVerifier(map[string]ed25519.PublicKey{"some-other-key": pub}, testIssuer, testAudience, 0, time.Now)
	if _, err := verifier.Verify(signed); !errors.Is(err, token.ErrUnknownKey) {
		t.Errorf("unknown kid: err = %v, want ErrUnknownKey", err)
	}
}

func TestIssueUsesAFreshJTI(t *testing.T) {
	pub, priv := mustKey(t)
	issuer := testIssuerFor(priv, time.Now)
	verifier := testVerifier(issuer.Kid(), pub, 0, time.Now)

	first, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue first: %v", err)
	}
	second, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue second: %v", err)
	}

	firstClaims, err := verifier.Verify(first)
	if err != nil {
		t.Fatalf("Verify first: %v", err)
	}
	secondClaims, err := verifier.Verify(second)
	if err != nil {
		t.Fatalf("Verify second: %v", err)
	}
	if firstClaims.ID == "" || secondClaims.ID == "" {
		t.Fatal("jti must not be empty")
	}
	if firstClaims.ID == secondClaims.ID {
		t.Errorf("two issues reused jti %q", firstClaims.ID)
	}
	// 128 bits, base64url without padding, is 22 characters.
	if len(firstClaims.ID) != 22 {
		t.Errorf("jti %q is %d characters, want 22", firstClaims.ID, len(firstClaims.ID))
	}
}

func TestWriteRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	_, priv := mustKey(t)
	if err := token.Write(path, priv); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	_, other := mustKey(t)
	if err := token.Write(path, other); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second Write: err = %v, want fs.ErrExist", err)
	}

	loaded, err := token.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Equal(priv) {
		t.Error("the refused second Write clobbered the first key")
	}
}

func TestLoadRejectsGroupReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not report POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	_, priv := mustKey(t)
	if err := token.Write(path, priv); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if _, err := token.Load(path); err == nil {
		t.Fatal("Load accepted a file readable by group and other")
	}
}

func TestKeyFileIssueVerifyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	pub, priv := mustKey(t)
	if err := token.Write(path, priv); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := token.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	issuer := testIssuerFor(loaded, time.Now)
	signed, _, err := issuer.Issue(testPlayerID, testAllocationID, testServerID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := testVerifier(issuer.Kid(), pub, 0, time.Now).Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != testPlayerID || claims.Alloc != testAllocationID || claims.Srv != testServerID {
		t.Errorf("claims = %+v, want player/allocation/server round-tripped", claims)
	}
}

func TestThumbprintMatchesRFC8037(t *testing.T) {
	const (
		x    = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
		want = "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"
	)
	raw, err := base64.RawURLEncoding.DecodeString(x)
	if err != nil {
		t.Fatalf("decode RFC 8037 x: %v", err)
	}
	if got := token.Thumbprint(ed25519.PublicKey(raw)); got != want {
		t.Errorf("Thumbprint = %q, want %q", got, want)
	}
}

func TestJWKSIsByteStableAndParses(t *testing.T) {
	pub, _ := mustKey(t)
	first := token.JWKS(pub)
	second := token.JWKS(pub)
	if !bytes.Equal(first, second) {
		t.Fatalf("JWKS is not byte-stable:\n%s\n%s", first, second)
	}

	var set token.JWKSet
	if err := json.Unmarshal(first, &set); err != nil {
		t.Fatalf("unmarshal JWKS: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("len(keys) = %d, want 1", len(set.Keys))
	}
	key := set.Keys[0]
	if key.Kty != "OKP" || key.Crv != "Ed25519" || key.Use != "sig" || key.Alg != "EdDSA" {
		t.Errorf("key attributes = %+v, want OKP/Ed25519/sig/EdDSA", key)
	}
	if key.Kid != token.Thumbprint(pub) {
		t.Errorf("kid = %q, want %q", key.Kid, token.Thumbprint(pub))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		t.Fatalf("x is not base64url: %v", err)
	}
	if !ed25519.PublicKey(decoded).Equal(pub) {
		t.Error("x does not decode to the key that was passed in")
	}

	want := `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"` + key.Kid + `","x":"` + key.X + `","use":"sig","alg":"EdDSA"}]}`
	if string(first) != want {
		t.Errorf("JWKS = %s, want %s", first, want)
	}
}

func TestHandlerServesJWKS(t *testing.T) {
	pub, _ := mustKey(t)
	doc := token.JWKS(pub)
	server := httptest.NewServer(token.Handler(doc))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("GET Cache-Control = %q, want public, max-age=300", cc)
	}
	if !bytes.Equal(body, doc) {
		t.Errorf("GET body = %s, want %s", body, doc)
	}

	req, err := http.NewRequest(http.MethodHead, server.URL, nil)
	if err != nil {
		t.Fatalf("new HEAD request: %v", err)
	}
	head, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	headBody, _ := io.ReadAll(head.Body)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", head.StatusCode)
	}
	if ct := head.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("HEAD Content-Type = %q, want application/json", ct)
	}
	if len(headBody) != 0 {
		t.Errorf("HEAD body = %q, want empty", headBody)
	}

	post, err := http.Post(server.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", post.StatusCode)
	}
}
