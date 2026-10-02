package server

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/patch/internal/blob"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
	"github.com/otomo-live/otomo/services/patch/internal/servicekey"
)

// newKey writes a fresh 32-byte base64url key file and returns its path and text.
func newKey(t *testing.T) (path, text string) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	text = base64.RawURLEncoding.EncodeToString(b)
	path = filepath.Join(t.TempDir(), "patch_session.key")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, text
}

// serverManifestHarness serves a live release whose server manifest was never stored
// (NULL), and one blob, on a server that accepts the returned Session key.
func serverManifestHarness(t *testing.T, keys bool) (h *harness, key string, blobSHA string) {
	t.Helper()

	client := []byte(`{"channel":"live","config":{"ui.motd":{"sha256":"ab","size":1,"version":1}},"created_at":"2026-09-29T00:00:00Z","format":1,"min_client_version":"1.0.0","packs":[],"release_id":42}`)
	server, err := manifest.EmptyServerManifest(client)
	if err != nil {
		t.Fatal(err)
	}
	holder := manifest.NewHolder(manifest.NewSet(map[string]*manifest.Entry{
		"live": {Channel: "live", ReleaseID: 42, Body: client, ETag: `"client"`, MinClientVersion: "1.0.0", Server: server},
	}))

	root := t.TempDir()
	blobSHA = writeBlob(t, root, []byte("server namespace document"))
	br, err := blob.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{ManifestHolder: holder, BlobRoot: br}
	if keys {
		path, text := newKey(t)
		k, err := servicekey.LoadKeys(map[servicekey.Role]string{servicekey.RoleSession: path})
		if err != nil {
			t.Fatal(err)
		}
		deps.ServiceKeys, key = k, text
	}
	return newHarnessWithDeps(t, deps), key, blobSHA
}

// get sends a GET with the given headers and returns the status, headers and body.
func (h *harness) get(t *testing.T, base, path string, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, headers: resp.Header, body: string(body)}
}

const serverManifestPath = "/internal/patch/server-manifest/live"

// TestServerManifestNeedsTheSessionKey: a missing, malformed or wrong key is 401.
func TestServerManifestNeedsTheSessionKey(t *testing.T) {
	h, key, _ := serverManifestHarness(t, true)
	_, other := newKey(t)

	for name, auth := range map[string]string{
		"missing":    "",
		"malformed":  "Basic " + key,
		"wrong key":  "Bearer " + other,
		"not base64": "Bearer !!!",
	} {
		headers := map[string]string{}
		if auth != "" {
			headers["Authorization"] = auth
		}
		if got := h.get(t, h.api, serverManifestPath, headers); got.status != http.StatusUnauthorized {
			t.Errorf("%s key = %d %s, want 401", name, got.status, got.body)
		}
	}
}

// TestServerManifestServesAnEmptyManifestForANullRow: the release has no stored server
// manifest, which is an empty manifest with ETag support, not an error.
func TestServerManifestServesAnEmptyManifestForANullRow(t *testing.T) {
	h, key, _ := serverManifestHarness(t, true)
	auth := map[string]string{"Authorization": "Bearer " + key}

	got := h.get(t, h.api, serverManifestPath, auth)
	if got.status != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200", got.status, got.body)
	}
	want := `{"channel":"live","config":{},"created_at":"2026-09-29T00:00:00Z","format":1,"min_client_version":"1.0.0","packs":[],"release_id":42}`
	if got.body != want {
		t.Errorf("body = %s\nwant   %s", got.body, want)
	}
	etag := got.headers.Get("ETag")
	if etag == "" || etag == `"client"` {
		t.Errorf("ETag = %q, want the server manifest's own", etag)
	}
	if got.headers.Get("X-Otomo-Release") != "42" || got.headers.Get("Cache-Control") != "no-cache" {
		t.Errorf("headers = %v", got.headers)
	}

	auth["If-None-Match"] = etag
	if again := h.get(t, h.api, serverManifestPath, auth); again.status != http.StatusNotModified || again.body != "" {
		t.Errorf("GET with the ETag = %d %q, want 304 and no body", again.status, again.body)
	}
}

func TestServerManifestUnknownChannel(t *testing.T) {
	h, key, _ := serverManifestHarness(t, true)
	got := h.get(t, h.api, "/internal/patch/server-manifest/nope", map[string]string{"Authorization": "Bearer " + key})
	if got.status != http.StatusNotFound {
		t.Errorf("unknown channel = %d, want 404", got.status)
	}
}

// TestServerBlobsNeedTheKey: the server manifest's blobs are on the internal listener
// behind the same key.
func TestServerBlobsNeedTheKey(t *testing.T) {
	h, key, sha := serverManifestHarness(t, true)
	path := "/internal/patch/blob/" + sha

	if got := h.get(t, h.api, path, nil); got.status != http.StatusUnauthorized {
		t.Errorf("blob without a key = %d, want 401", got.status)
	}
	got := h.get(t, h.api, path, map[string]string{"Authorization": "Bearer " + key})
	if got.status != http.StatusOK || got.body != "server namespace document" {
		t.Errorf("blob with the key = %d %q", got.status, got.body)
	}
}

// TestServerManifestIsOnlyOnTheInternalListener: the public listener, which both
// gateways route to, and the metrics listener do not serve it.
func TestServerManifestIsOnlyOnTheInternalListener(t *testing.T) {
	h, key, _ := serverManifestHarness(t, true)
	auth := map[string]string{"Authorization": "Bearer " + key}

	for name, base := range map[string]string{"public": h.public, "metrics": h.internal} {
		for _, path := range []string{serverManifestPath, "/patch/v1/live/server-manifest", "/patch" + serverManifestPath} {
			if got := h.get(t, base, path, auth); got.status != http.StatusNotFound {
				t.Errorf("%s listener %s = %d, want 404", name, path, got.status)
			}
		}
	}
	// And the internal listener serves nothing else.
	if got := h.get(t, h.api, "/patch/v1/live/manifest", auth); got.status != http.StatusNotFound {
		t.Errorf("client manifest on the internal listener = %d, want 404", got.status)
	}
}

// TestNoKeysRefuseEveryone: a Patch started without PATCH_SESSION_KEY_PATH fails closed.
func TestNoKeysRefuseEveryone(t *testing.T) {
	h, _, _ := serverManifestHarness(t, false)
	_, some := newKey(t)
	if got := h.get(t, h.api, serverManifestPath, map[string]string{"Authorization": "Bearer " + some}); got.status != http.StatusUnauthorized {
		t.Errorf("with no keys configured = %d, want 401", got.status)
	}
	if !strings.Contains(h.api, "127.0.0.1:") {
		t.Fatalf("api address %q", h.api)
	}
}
