//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

const (
	testIssuer   = "https://admin-auth.otomo.internal"
	testAudience = "otomo:staff"
	testKid      = "integration-staff-key"

	// devHeadLockKey is the key Config's own tests take the same advisory lock on.
	// Holding it serialises this test against every other package that moves the dev
	// head, which matters because channel_head is shared global state.
	devHeadLockKey = "config_test:dev"

	readyTimeout = 30 * time.Second
	propTimeout  = 5 * time.Second
	pollEvery    = 100 * time.Millisecond
)

// TestPublishRollback drives the whole Config -> Patch path over real binaries: a
// publish becomes a new manifest and ETag within seconds, a conditional request
// revalidates, a second publish moves the manifest again, and a rollback returns the
// exact bytes the first publish served.
func TestPublishRollback(t *testing.T) {
	dbURL := os.Getenv("PATCH_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("PATCH_TEST_DATABASE_URL not set; skipping the Config -> Patch integration test")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	moduleRoot := filepath.Dir(wd)
	configDir := filepath.Join(moduleRoot, "..", "config")

	binDir := t.TempDir()
	patchBin := buildBinary(t, moduleRoot, binDir, "patch")
	configBin := buildBinary(t, configDir, binDir, "config")

	// Staff key material, in-process: an Ed25519 key, its public half in a JWKS the
	// services fetch over HTTP, and tokens signed with the private half.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"kid": testKid,
		"alg": "EdDSA",
		"use": "sig",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
	}}}
	jwksBody, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBody)
	}))
	t.Cleanup(jwksSrv.Close)

	adminToken := signToken(t, priv, []string{"admin"})
	liveOpsToken := signToken(t, priv, []string{"live_ops"})

	runMigrate(t, configBin, dbURL)

	ports := pickPorts(t, 4)
	configPublic, configMetrics := ports[0], ports[1]
	patchPublic, patchMetrics := ports[2], ports[3]

	blobRoot := t.TempDir()

	configEnv := mergeEnv(os.Environ(),
		"CONFIG_DATABASE_URL="+dbURL,
		"CONFIG_BLOB_ROOT="+blobRoot,
		"CONFIG_STAFF_JWKS_URL="+jwksSrv.URL+"/.well-known/jwks.json",
		"CONFIG_STAFF_ISSUER="+testIssuer,
		"CONFIG_STAFF_AUDIENCE="+testAudience,
		"CONFIG_LISTEN_ADDR="+configPublic,
		"CONFIG_METRICS_ADDR="+configMetrics,
		"CONFIG_LOG_LEVEL=debug",
	)
	patchEnv := mergeEnv(os.Environ(),
		"PATCH_DATABASE_URL="+dbURL,
		"PATCH_BLOB_ROOT="+blobRoot,
		"PATCH_STAFF_JWKS_URL="+jwksSrv.URL+"/.well-known/jwks.json",
		"PATCH_STAFF_ISSUER="+testIssuer,
		"PATCH_STAFF_AUDIENCE="+testAudience,
		"PATCH_LISTEN_ADDR="+patchPublic,
		"PATCH_METRICS_ADDR="+patchMetrics,
		"PATCH_POLL_INTERVAL=2s",
		"PATCH_LOG_LEVEL=debug",
	)

	configProc := startProcess(t, "config", configDir, configBin, []string{"serve"}, configEnv)
	patchProc := startProcess(t, "patch", moduleRoot, patchBin, []string{"serve"}, patchEnv)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("config output:\n%s", configProc.out.String())
			t.Logf("patch output:\n%s", patchProc.out.String())
		}
	})

	client := &http.Client{Timeout: 10 * time.Second}
	waitReady(t, "http://"+configMetrics+"/readyz", configProc, readyTimeout)
	waitReady(t, "http://"+patchMetrics+"/readyz", patchProc, readyTimeout)

	configBase := "http://" + configPublic
	patchBase := "http://" + patchPublic

	// Serialise with the rest of the test suite that moves the dev head, then read the
	// head we must put back afterward.
	ctx := context.Background()
	lockConn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect for advisory lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1)::bigint)`, devHeadLockKey); err != nil {
		_ = lockConn.Close(ctx)
		t.Fatalf("advisory lock %s: %v", devHeadLockKey, err)
	}
	var originalDevHead int64
	t.Cleanup(func() {
		_ = restoreDevHead(lockConn, originalDevHead)
		_ = lockConn.Close(context.Background())
	})

	originalDevHead = readDevHead(t, client, configBase, adminToken)

	// Build a fresh, unique namespace through Config's HTTP API only.
	ns := fmt.Sprintf("it.patch_%d", time.Now().UnixNano())
	postJSON(t, client, configBase+"/api/admin/config/namespaces", adminToken,
		fmt.Sprintf(`{"name":%q,"audience":"client","description":"patch integration"}`, ns),
		http.StatusCreated)

	put(t, client, configBase+"/api/admin/config/namespaces/"+ns+"/schema", adminToken,
		`{"type":"object"}`, http.StatusOK)

	revision := saveDraft(t, client, configBase, liveOpsToken, ns, `{"n":1}`, 1)
	v1 := createVersion(t, client, configBase, liveOpsToken, ns, revision, "v1")

	revision = saveDraft(t, client, configBase, liveOpsToken, ns, `{"n":2}`, revision)
	v2 := createVersion(t, client, configBase, liveOpsToken, ns, revision, "v2")

	// E0: the bootstrap manifest the migration seeded.
	e0, _ := getManifest(t, client, patchBase, liveOpsToken, "")
	if e0 == "" {
		t.Fatalf("manifest for dev has no ETag: %s\n\n%s", patchProc.out.String(), configProc.out.String())
	}

	// Publish v1 and wait for Patch to serve it.
	rel1 := publish(t, client, configBase, liveOpsToken, "dev", originalDevHead, ns, v1, "publish v1")
	e1, e1Body := waitManifestVersion(t, client, patchBase, liveOpsToken, e0, ns, v1, patchProc)

	// A matching If-None-Match revalidates with an empty 304.
	status, body, _ := doRequest(t, client, http.MethodGet, patchBase+"/patch/v1/dev/manifest", liveOpsToken, "", map[string]string{
		"If-None-Match": e1,
	})
	if status != http.StatusNotModified {
		t.Fatalf("If-None-Match %s = %d, want 304: %s", e1, status, body)
	}
	if len(body) != 0 {
		t.Fatalf("304 carries a %d-byte body, want empty", len(body))
	}

	// Publish v2 and wait for the second manifest.
	rel2 := publish(t, client, configBase, liveOpsToken, "dev", rel1, ns, v2, "publish v2")
	e2, _ := waitManifestVersion(t, client, patchBase, liveOpsToken, e1, ns, v2, patchProc)
	if e2 == e1 {
		t.Fatalf("manifest ETag did not change after v2 publish: still %s", e2)
	}

	// Roll back dev to v1: the canonical manifest is deterministic, so Patch must serve
	// the exact bytes (and content-derived ETag) it served before.
	postJSON(t, client, configBase+"/api/admin/config/channels/dev/rollback", adminToken,
		fmt.Sprintf(`{"release_id":%d,"base_release_id":%d}`, rel1, rel2),
		http.StatusOK)

	deadline := time.Now().Add(propTimeout)
	for {
		etag, manifest := getManifest(t, client, patchBase, liveOpsToken, "")
		if etag == e1 && bytes.Equal(manifest, e1Body) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollback did not restore the v1 manifest within %s (etag %s)\n\npatch:\n%s",
				propTimeout, etag, patchProc.out.String())
		}
		time.Sleep(pollEvery)
	}

	// The namespace's blob is fetchable at its hash and the bytes hash back to it.
	sha := manifestConfigSHA(t, e1Body, ns)
	status, body, _ = doRequest(t, client, http.MethodGet, patchBase+"/patch/v1/blob/"+sha, "", "", nil)
	if status != http.StatusOK {
		t.Fatalf("blob %s = %d, want 200: %s", sha, status, body)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != sha {
		t.Fatalf("blob %s hashes to %s", sha, got)
	}
}

// ---------------------------------------------------------------- key material --

type staffClaims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
	Name  string   `json:"name,omitempty"`
}

func signToken(t *testing.T, priv ed25519.PrivateKey, roles []string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, staffClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			Subject:   "staff-integration-1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
		},
		Roles: roles,
		Name:  "Integration Tester",
	})
	tok.Header["kid"] = testKid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign %v token: %v", roles, err)
	}
	return s
}

// ------------------------------------------------------------------- binaries --

func buildBinary(t *testing.T, dir, outDir, name string) string {
	t.Helper()
	out := filepath.Join(outDir, name)
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build %s: %v\n%s", dir, err, buf.String())
	}
	return out
}

func runMigrate(t *testing.T, configBin, dbURL string) {
	t.Helper()
	cmd := exec.Command(configBin, "migrate")
	cmd.Env = mergeEnv(os.Environ(), "CONFIG_DATABASE_URL="+dbURL)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("config migrate: %v\n%s", err, buf.String())
	}
}

// ---------------------------------------------------------------- processes --

type childProcess struct {
	name string
	cmd  *exec.Cmd
	out  *lockedBuffer
	done chan struct{}

	mu      sync.Mutex
	waitErr error
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startProcess(t *testing.T, name, dir, bin string, args, env []string) *childProcess {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &childProcess{name: name, cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	t.Cleanup(func() { p.stop() })
	return p
}

func (p *childProcess) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *childProcess) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func waitReady(t *testing.T, url string, proc *childProcess, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-proc.done:
			t.Fatalf("%s exited before ready: %v\n%s", proc.name, proc.err(), proc.out.String())
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("%s not ready within %s\n%s", proc.name, timeout, proc.out.String())
}

// ---------------------------------------------------------------- HTTP helpers --

func doRequest(t *testing.T, client *http.Client, method, url, token, body string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}
	return resp.StatusCode, b, resp.Header
}

func postJSON(t *testing.T, client *http.Client, url, token, body string, want int) {
	t.Helper()
	status, got, _ := doRequest(t, client, http.MethodPost, url, token, body, nil)
	if status != want {
		t.Fatalf("POST %s = %d, want %d: %s", url, status, want, got)
	}
}

func put(t *testing.T, client *http.Client, url, token, body string, want int) {
	t.Helper()
	status, got, _ := doRequest(t, client, http.MethodPut, url, token, body, nil)
	if status != want {
		t.Fatalf("PUT %s = %d, want %d: %s", url, status, want, got)
	}
}

func saveDraft(t *testing.T, client *http.Client, configBase, token, ns, document string, revision int) int {
	t.Helper()
	body := fmt.Sprintf(`{"document":%s,"revision":%d}`, document, revision)
	status, got, _ := doRequest(t, client, http.MethodPut,
		configBase+"/api/admin/config/namespaces/"+ns+"/draft", token, body, nil)
	if status != http.StatusOK {
		t.Fatalf("save draft = %d: %s", status, got)
	}
	var saved struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(got, &saved); err != nil {
		t.Fatalf("decode save draft: %v (%s)", err, got)
	}
	return saved.Revision
}

func createVersion(t *testing.T, client *http.Client, configBase, token, ns string, revision int, message string) int {
	t.Helper()
	body := fmt.Sprintf(`{"message":%q,"revision":%d}`, message, revision)
	status, got, _ := doRequest(t, client, http.MethodPost,
		configBase+"/api/admin/config/namespaces/"+ns+"/versions", token, body, nil)
	if status != http.StatusCreated {
		t.Fatalf("create version = %d: %s", status, got)
	}
	var v struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("decode version: %v (%s)", err, got)
	}
	return v.Version
}

func publish(t *testing.T, client *http.Client, configBase, token, channel string, base int64, ns string, version int, message string) int64 {
	t.Helper()
	body := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[],"min_client_version":"1.0.0","message":%q}`,
		base, ns, version, message)
	status, got, _ := doRequest(t, client, http.MethodPost,
		configBase+"/api/admin/config/channels/"+channel+"/releases", token, body, nil)
	if status != http.StatusCreated {
		t.Fatalf("publish %s = %d: %s", channel, status, got)
	}
	var rel struct {
		ReleaseID int64 `json:"release_id"`
	}
	if err := json.Unmarshal(got, &rel); err != nil {
		t.Fatalf("decode release: %v (%s)", err, got)
	}
	return rel.ReleaseID
}

func readDevHead(t *testing.T, client *http.Client, configBase, token string) int64 {
	t.Helper()
	status, got, _ := doRequest(t, client, http.MethodGet,
		configBase+"/api/admin/config/channels/dev/releases", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("list dev releases = %d: %s", status, got)
	}
	var list struct {
		HeadReleaseID int64 `json:"head_release_id"`
	}
	if err := json.Unmarshal(got, &list); err != nil {
		t.Fatalf("decode releases: %v (%s)", err, got)
	}
	return list.HeadReleaseID
}

// getManifest returns the dev manifest's ETag and body. The body is nil on a 304 or
// an error, which the polling loop treats as "not the wanted state yet".
func getManifest(t *testing.T, client *http.Client, patchBase, token, ifNoneMatch string) (string, []byte) {
	t.Helper()
	headers := map[string]string{}
	if ifNoneMatch != "" {
		headers["If-None-Match"] = ifNoneMatch
	}
	status, body, hdr := doRequest(t, client, http.MethodGet, patchBase+"/patch/v1/dev/manifest", token, "", headers)
	if status == http.StatusNotModified {
		return hdr.Get("ETag"), nil
	}
	if status != http.StatusOK {
		return "", nil
	}
	return hdr.Get("ETag"), body
}

// waitManifestVersion polls until Patch serves ns at version with an ETag different
// from previous, and returns that body.
func waitManifestVersion(t *testing.T, client *http.Client, patchBase, token, previous, ns string, version int, patchProc *childProcess) (string, []byte) {
	t.Helper()
	deadline := time.Now().Add(propTimeout)
	for {
		etag, body := getManifest(t, client, patchBase, token, "")
		if etag != "" && etag != previous {
			cfg, ok := manifestConfig(body, ns)
			if ok && cfg.Version == version {
				return etag, body
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not serve %s v%d within %s\n\npatch:\n%s",
				ns, ns, version, propTimeout, patchProc.out.String())
		}
		time.Sleep(pollEvery)
	}
}

type manifestConfigEntry struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

func manifestConfig(body []byte, ns string) (manifestConfigEntry, bool) {
	var doc struct {
		Config map[string]manifestConfigEntry `json:"config"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return manifestConfigEntry{}, false
	}
	e, ok := doc.Config[ns]
	return e, ok
}

func manifestConfigSHA(t *testing.T, body []byte, ns string) string {
	t.Helper()
	e, ok := manifestConfig(body, ns)
	if !ok || e.SHA256 == "" {
		t.Fatalf("manifest has no config entry for %s: %s", ns, body)
	}
	return e.SHA256
}

// ------------------------------------------------------------------ database --

func restoreDevHead(conn *pgx.Conn, originalHead int64) error {
	ctx := context.Background()
	if _, err := conn.Exec(ctx,
		`UPDATE channel_head SET release_id = $1, updated_by = 'integration-cleanup', updated_at = now()
		  WHERE channel = 'dev'`, originalHead); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_notify('config_release', 'dev')`); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1)::bigint)`, devHeadLockKey)
	return err
}

// --------------------------------------------------------------- small helpers --

func pickPorts(t *testing.T, n int) []string {
	t.Helper()
	seen := make(map[string]bool, n)
	out := make([]string, 0, n)
	for len(out) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick a free port: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

// mergeEnv returns base with any variable named in extra removed, then extra appended.
// That keeps the child from seeing a stale value the test process inherited.
func mergeEnv(base []string, extra ...string) []string {
	drop := make(map[string]bool, len(extra))
	for _, kv := range extra {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			drop[kv[:i]] = true
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if !drop[key] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
