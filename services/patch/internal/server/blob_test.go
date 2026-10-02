package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/patch/internal/blob"
	"github.com/otomo-live/otomo/services/patch/internal/manifest"
)

// blobTestRoot builds the private parent, the blob root under it and a sentinel file
// one directory above the root (plus a decoy at the shape a two-level traversal would
// reach). Every 400/404 test asserts the sentinel's bytes never appear in a response
// body: an escaping open would have to surface them.
func blobTestRoot(t *testing.T) (root, sentinel string) {
	t.Helper()

	base := t.TempDir()
	root = filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir blob root: %v", err)
	}

	const marker = "SENTINEL:CANARY:DO-NOT-SERVE"
	sentinel = marker
	if err := os.WriteFile(filepath.Join(base, "sentinel"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir decoy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "etc", "passwd"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	return root, marker
}

// blobPath is the layout Config's writer uses: <root>/blobs/<sha[0:2]>/<sha[2:4]>/<sha>.
func blobPath(root, sha string) string {
	return filepath.Join(root, "blobs", sha[0:2], sha[2:4], sha)
}

func writeBlobAt(t *testing.T, root, sha string, content []byte) {
	t.Helper()

	p := blobPath(root, sha)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
}

// writeBlob stores content under its real sha256, exactly as Config would.
func writeBlob(t *testing.T, root string, content []byte) string {
	t.Helper()

	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	writeBlobAt(t, root, sha, content)
	return sha
}

func newBlobHarness(t *testing.T, root string) *harness {
	t.Helper()

	br, err := blob.NewRoot(root)
	if err != nil {
		t.Fatalf("blob.NewRoot: %v", err)
	}
	return newHarnessWithDeps(t, Deps{BlobRoot: br})
}

// doMethod is h.do for a method other than GET, which the HEAD test needs.
func (h *harness) doMethod(t *testing.T, base, path, method string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+base+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, headers: resp.Header, body: string(body)}
}

func TestBlobServesFullBodyWithHeaders(t *testing.T) {
	root, _ := blobTestRoot(t)
	content := []byte("hello content-addressed blob")
	sha := writeBlob(t, root, content)
	h := newBlobHarness(t, root)

	resp := h.do(t, h.public, "/patch/v1/blob/"+sha)
	if resp.status != http.StatusOK {
		t.Fatalf("GET blob = %d, want 200 (%s)", resp.status, resp.body)
	}
	if resp.body != string(content) {
		t.Errorf("body = %q, want %q", resp.body, content)
	}
	for header, want := range map[string]string{
		"Content-Type":   "application/octet-stream",
		"Cache-Control":  "public, max-age=31536000, immutable",
		"ETag":           `"` + sha + `"`,
		"Content-Length": strconv.Itoa(len(content)),
	} {
		if got := resp.headers.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestBlobRangeRequests(t *testing.T) {
	root, _ := blobTestRoot(t)
	data := make([]byte, 2048)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sha := writeBlob(t, root, data)
	h := newBlobHarness(t, root)
	path := "/patch/v1/blob/" + sha

	t.Run("head range", func(t *testing.T) {
		resp := h.doWithHeader(t, h.public, path, "Range", "bytes=0-1023")
		if resp.status != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206 (%s)", resp.status, resp.body)
		}
		if got := resp.headers.Get("Content-Range"); got != "bytes 0-1023/2048" {
			t.Errorf("Content-Range = %q, want %q", got, "bytes 0-1023/2048")
		}
		if len(resp.body) != 1024 {
			t.Errorf("body length = %d, want 1024", len(resp.body))
		}
		if resp.body != string(data[:1024]) {
			t.Error("body does not match the requested byte range")
		}
	})

	t.Run("tail range", func(t *testing.T) {
		resp := h.doWithHeader(t, h.public, path, "Range", "bytes=100-")
		if resp.status != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206 (%s)", resp.status, resp.body)
		}
		if got := resp.headers.Get("Content-Range"); got != "bytes 100-2047/2048" {
			t.Errorf("Content-Range = %q, want %q", got, "bytes 100-2047/2048")
		}
		if len(resp.body) != 1948 {
			t.Errorf("body length = %d, want 1948", len(resp.body))
		}
		if resp.body != string(data[100:]) {
			t.Error("body does not match the requested tail")
		}
	})

	t.Run("unsatisfiable", func(t *testing.T) {
		resp := h.doWithHeader(t, h.public, path, "Range", "bytes=999999-")
		if resp.status != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d, want 416", resp.status)
		}
	})
}

func TestBlobRevalidationReturns304(t *testing.T) {
	root, _ := blobTestRoot(t)
	sha := writeBlob(t, root, []byte("revalidate me"))
	h := newBlobHarness(t, root)

	resp := h.doWithHeader(t, h.public, "/patch/v1/blob/"+sha, "If-None-Match", `"`+sha+`"`)
	if resp.status != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 (%s)", resp.status, resp.body)
	}
	if resp.body != "" {
		t.Errorf("304 body = %q, want empty", resp.body)
	}
	if got := resp.headers.Get("ETag"); got != `"`+sha+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+sha+`"`)
	}
}

func TestBlobHEADCarriesHeadersWithoutBody(t *testing.T) {
	root, _ := blobTestRoot(t)
	content := []byte("head me")
	sha := writeBlob(t, root, content)
	h := newBlobHarness(t, root)

	resp := h.doMethod(t, h.public, "/patch/v1/blob/"+sha, http.MethodHead)
	if resp.status != http.StatusOK {
		t.Fatalf("HEAD blob = %d, want 200", resp.status)
	}
	if resp.body != "" {
		t.Errorf("HEAD body = %q, want empty", resp.body)
	}
	if got := resp.headers.Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
	if got := resp.headers.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := resp.headers.Get("ETag"); got != `"`+sha+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+sha+`"`)
	}
}

func TestBlobNotFound(t *testing.T) {
	root, sentinel := blobTestRoot(t)
	h := newBlobHarness(t, root)

	unknown := strings.Repeat("a", 64)
	dir := strings.Repeat("b", 64)
	if err := os.MkdirAll(blobPath(root, dir), 0o755); err != nil {
		t.Fatalf("mkdir directory at blob path: %v", err)
	}

	for name, sha := range map[string]string{"unknown hash": unknown, "directory at path": dir} {
		t.Run(name, func(t *testing.T) {
			resp := h.do(t, h.public, "/patch/v1/blob/"+sha)
			if resp.status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", resp.status, resp.body)
			}
			if got := resp.code(t); got != "not_found" {
				t.Errorf("code = %q, want not_found", got)
			}
			if strings.Contains(resp.body, sentinel) {
				t.Errorf("response leaked a file outside the blob root: %s", resp.body)
			}
		})
	}
}

func TestBlobRejectsInvalidSHA(t *testing.T) {
	root, sentinel := blobTestRoot(t)
	h := newBlobHarness(t, root)

	valid := strings.Repeat("a", 64)
	cases := map[string]string{
		"63 hex chars":      "/patch/v1/blob/" + strings.Repeat("a", 63),
		"uppercase hex":     "/patch/v1/blob/" + strings.ToUpper(valid),
		"literal traversal": "/patch/v1/blob/../../etc/passwd",
		"encoded traversal": "/patch/v1/blob/..%2F..%2Fetc%2Fpasswd",
		"trailing segment":  "/patch/v1/blob/" + valid + "%2Fx",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.do(t, h.public, path)
			if resp.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", resp.status, resp.body)
			}
			if got := resp.code(t); got != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", got)
			}
			if !strings.Contains(resp.body, "sha256 must match ^[0-9a-f]{64}$") {
				t.Errorf("body does not carry the required message: %s", resp.body)
			}
			if strings.Contains(resp.body, sentinel) {
				t.Errorf("response leaked a file outside the blob root: %s", resp.body)
			}
		})
	}
}

func TestBlobMetricsCountPerResult(t *testing.T) {
	root, _ := blobTestRoot(t)
	data := bytes.Repeat([]byte("x"), 4096)
	sha := writeBlob(t, root, data)
	h := newBlobHarness(t, root)
	path := "/patch/v1/blob/" + sha

	if r := h.do(t, h.public, path); r.status != http.StatusOK {
		t.Fatalf("full GET = %d, want 200", r.status)
	}
	if r := h.doWithHeader(t, h.public, path, "Range", "bytes=0-99"); r.status != http.StatusPartialContent {
		t.Fatalf("range GET = %d, want 206", r.status)
	}
	if r := h.doWithHeader(t, h.public, path, "If-None-Match", `"`+sha+`"`); r.status != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", r.status)
	}
	if r := h.do(t, h.public, "/patch/v1/blob/"+strings.Repeat("a", 64)); r.status != http.StatusNotFound {
		t.Fatalf("unknown blob = %d, want 404", r.status)
	}
	if r := h.do(t, h.public, "/patch/v1/blob/"+strings.Repeat("a", 63)); r.status != http.StatusBadRequest {
		t.Fatalf("invalid sha = %d, want 400", r.status)
	}
	if r := h.doWithHeader(t, h.public, path, "Range", "bytes=999999-"); r.status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range = %d, want 416", r.status)
	}

	metrics := h.do(t, h.internal, "/metrics").body
	for _, result := range []string{"200", "206", "304", "404", "400"} {
		series := `patch_blob_requests_total{result="` + result + `"} 1`
		if !strings.Contains(metrics, series) {
			t.Errorf("/metrics does not contain %q", series)
		}
	}
	if strings.Contains(metrics, `patch_blob_requests_total{result="416"}`) {
		t.Error("416 must not be a patch_blob_requests_total label")
	}
}

// TestBlobRouteAbsentWithoutRoot pins the zero-value Deps behaviour: with no blob
// root the route is not registered, so a public blob request falls through to the
// COM-5 catch-all.
func TestBlobRouteAbsentWithoutRoot(t *testing.T) {
	h := newHarness(t, nil, nil)
	resp := h.do(t, h.public, "/patch/v1/blob/"+strings.Repeat("a", 64))
	if resp.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.status)
	}
	if got := resp.code(t); got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}
}

// TestBlobAndManifestRoutesCoexist is the regression guard for the ServeMux
// conflict: the blob wildcard and the manifest wildcard both match
// /patch/v1/blob/manifest, so registering them side by side on one mux would panic
// at start-up, exactly as main wires them.
func TestBlobAndManifestRoutesCoexist(t *testing.T) {
	root, _ := blobTestRoot(t)
	br, err := blob.NewRoot(root)
	if err != nil {
		t.Fatalf("blob.NewRoot: %v", err)
	}
	sha := writeBlob(t, root, []byte("coexist"))
	holder := manifest.NewHolder(manifest.NewSet(map[string]*manifest.Entry{
		"live": {Channel: "live", Body: []byte(`{"release":1}`), ETag: `"e"`, MinClientVersion: "1"},
	}))
	h := newHarnessWithDeps(t, Deps{BlobRoot: br, ManifestHolder: holder})

	if r := h.do(t, h.public, "/patch/v1/blob/"+sha); r.status != http.StatusOK {
		t.Fatalf("blob route = %d, want 200 (%s)", r.status, r.body)
	}
	if r := h.do(t, h.public, "/patch/v1/live/manifest"); r.status != http.StatusOK {
		t.Fatalf("manifest route = %d, want 200 (%s)", r.status, r.body)
	}
}

// TestBlobStreamsLargeFileWithBoundedAllocation serves a generated 32 MiB file and
// asserts the request allocated far less than the file size. ServeContent streams;
// a handler that read the file into memory would allocate at least 32 MiB here.
func TestBlobStreamsLargeFileWithBoundedAllocation(t *testing.T) {
	root, _ := blobTestRoot(t)
	const size = 32 << 20

	// Build the sparse file and name it by its real digest. Hashing happens before
	// the allocation measurement, so it cannot contaminate the result.
	tmp, err := os.CreateTemp(root, "large-*")
	if err != nil {
		t.Fatalf("create large temp: %v", err)
	}
	if err := tmp.Truncate(size); err != nil {
		t.Fatalf("truncate large temp: %v", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, tmp); err != nil {
		t.Fatalf("hash large temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close large temp: %v", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	dst := blobPath(root, sha)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir large blob dir: %v", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		t.Fatalf("commit large blob: %v", err)
	}

	h := newBlobHarness(t, root)
	client := &http.Client{Timeout: 60 * time.Second}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+h.public+"/patch/v1/blob/"+sha, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET large blob: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("stream body: %v", err)
	}
	if n != size {
		t.Fatalf("streamed %d bytes, want %d", n, size)
	}

	runtime.ReadMemStats(&after)
	if growth := after.TotalAlloc - before.TotalAlloc; growth >= 8<<20 {
		t.Errorf("streaming %d bytes allocated %d, want < %d", size, growth, 8<<20)
	}
}
