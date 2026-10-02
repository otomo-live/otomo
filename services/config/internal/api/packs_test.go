package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// fakePackStore is a DB-free PackStore. The pack handlers only need it to report what a
// real transaction would have written; the database contract itself is covered by the
// server package's DB-backed test.
type fakePackStore struct {
	mu    sync.Mutex
	packs []store.Pack
}

func (f *fakePackStore) CreatePack(_ context.Context, name string, ref blob.Ref, actor store.Entry) (store.Pack, bool, error) {
	p := store.Pack{
		PackID:     "00000000-0000-0000-0000-000000000001",
		Name:       name,
		SHA256:     ref.SHA256,
		Size:       ref.Size,
		UploadedBy: actor.ActorID,
		UploadedAt: time.Unix(0, 0).UTC(),
	}
	f.mu.Lock()
	f.packs = append(f.packs, p)
	f.mu.Unlock()
	return p, true, nil
}

func (f *fakePackStore) ListPacks(context.Context) ([]store.Pack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Pack(nil), f.packs...), nil
}

// packHandler wires the POST route with the given blob store, pack store and cap.
func packHandler(blobs *blob.Store, packs PackStore, maxBytes int64) http.Handler {
	return (&Handlers{Blobs: blobs, Packs: packs, MaxPackBytes: maxBytes}).
		For(Route{Method: http.MethodPost, Path: packsPath})
}

func countFiles(t *testing.T, root string) int {
	t.Helper()

	n := 0
	if err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

// TestCreatePackValidatesName checks the §5 name grammar at its edges: missing, empty,
// uppercase, a leading digit, a dash and one character past the 64-character limit.
func TestCreatePackValidatesName(t *testing.T) {
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler := packHandler(blobs, &fakePackStore{}, 1<<20)
	valid64 := "a" + strings.Repeat("b", 63)

	tests := []struct {
		name string
		want int
	}{
		{"", http.StatusBadRequest},
		{"Pack", http.StatusBadRequest},
		{"1pack", http.StatusBadRequest},
		{"has-dash", http.StatusBadRequest},
		{strings.Repeat("a", 65), http.StatusBadRequest},
		{"pack", http.StatusCreated},
		{"pack_name_2", http.StatusCreated},
		{valid64, http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				packsPath+"?name="+url.QueryEscape(tt.name), strings.NewReader("GDPCbody"))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.want, w.Body.String())
			}
			if tt.want == http.StatusBadRequest {
				if code, _ := errorEnvelope(t, w); code != "validation_failed" {
					t.Errorf("code = %q, want validation_failed", code)
				}
			}
		})
	}
}

// TestCreatePackRejectsNonGodotBodyWithoutWriting is the ordering contract: the magic
// is checked before the blob store is touched, so a non-pack body leaves no file in
// blobs/ or tmp/ and no temp file behind.
func TestCreatePackRejectsNonGodotBodyWithoutWriting(t *testing.T) {
	root := t.TempDir()
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler := packHandler(blobs, &fakePackStore{}, 1<<20)

	for _, body := range []string{"", "G", "NOPE", "GDP", "GDPX"} {
		t.Run(body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, packsPath+"?name=pack", strings.NewReader(body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			code, msg := errorEnvelope(t, w)
			if code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
			if !strings.Contains(msg, "GDPC") {
				t.Errorf("message %q does not name the missing header", msg)
			}
		})
	}
	if got := countFiles(t, root); got != 0 {
		t.Errorf("%d files left under the blob root after rejected bodies, want 0", got)
	}
}

// TestCreatePackOverCapLeavesNothingBehind checks that MaxBytesReader's error, which
// blob.Put wraps, is still recognised as a 413, and that the temp file Put opened is
// cleaned up.
func TestCreatePackOverCapLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler := packHandler(blobs, &fakePackStore{}, 16)

	req := httptest.NewRequest(http.MethodPost, packsPath+"?name=pack",
		strings.NewReader("GDPC"+strings.Repeat("x", 100)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", w.Code, w.Body.String())
	}
	if code, _ := errorEnvelope(t, w); code != "body_too_large" {
		t.Errorf("code = %q, want body_too_large", code)
	}
	if got := countFiles(t, root); got != 0 {
		t.Errorf("%d files left under the blob root after a capped body, want 0", got)
	}
}

// TestCreatePackStoresThePeekedHeader proves the bufio peek does not consume the magic:
// the bytes Put reads are the whole body, and the response's sha256 is the hash of
// exactly those bytes.
func TestCreatePackStoresThePeekedHeader(t *testing.T) {
	root := t.TempDir()
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var uploaded int64
	h := &Handlers{
		Blobs:        blobs,
		Packs:        &fakePackStore{},
		MaxPackBytes: 1 << 20,
		PackUpload:   func(n int64) { uploaded = n },
	}
	handler := h.For(Route{Method: http.MethodPost, Path: packsPath})

	body := "GDPCsmall-body"
	req := httptest.NewRequest(http.MethodPost, packsPath+"?name=small_pack", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	sum := sha256.Sum256([]byte(body))
	wantSHA := hex.EncodeToString(sum[:])
	if !strings.Contains(w.Body.String(), `"sha256":"`+wantSHA+`"`) {
		t.Errorf("response does not carry sha256 %s: %s", wantSHA, w.Body.String())
	}
	if uploaded != int64(len(body)) {
		t.Errorf("PackUpload got %d bytes, want %d", uploaded, len(body))
	}

	rc, size, err := blobs.Open(wantSHA)
	if err != nil {
		t.Fatalf("Open stored blob: %v", err)
	}
	defer rc.Close()
	stored, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	if string(stored) != body || size != int64(len(body)) {
		t.Errorf("stored blob = %q (%d bytes), want %q (%d bytes)", stored, size, body, len(body))
	}
	if got := countFiles(t, root); got != 1 {
		t.Errorf("%d files under the blob root, want the one committed blob", got)
	}
}

// packGenerator produces a GDPC body on the fly and hashes it as it goes, so the test
// never holds the body in memory.
type packGenerator struct {
	header    []byte
	headerOff int
	bodyLeft  int64
	pattern   [64]byte
	patternAt int
	h         hash.Hash
}

func newPackGenerator(bodySize int64) *packGenerator {
	g := &packGenerator{header: []byte(packMagic), bodyLeft: bodySize}
	g.h = sha256.New()
	for i := range g.pattern {
		g.pattern[i] = byte('a' + i%26)
	}
	return g
}

func (g *packGenerator) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if g.headerOff < len(g.header) {
			n += copy(p[n:], g.header[g.headerOff:])
			g.headerOff = len(g.header)
			continue
		}
		if g.bodyLeft == 0 {
			break
		}
		c := len(p) - n
		if int64(c) > g.bodyLeft {
			c = int(g.bodyLeft)
		}
		for i := 0; i < c; i++ {
			p[n+i] = g.pattern[g.patternAt]
			g.patternAt++
			if g.patternAt == len(g.pattern) {
				g.patternAt = 0
			}
		}
		g.bodyLeft -= int64(c)
		n += c
	}
	_, _ = g.h.Write(p[:n])
	if g.bodyLeft == 0 && g.headerOff == len(g.header) {
		return n, io.EOF
	}
	return n, nil
}

func (g *packGenerator) sha() string {
	return hex.EncodeToString(g.h.Sum(nil))
}

// TestCreatePackStreamsWithoutBuffering is the memory contract: a 64 MiB pack fed by a
// reader that produces bytes on demand must not be materialised. The request goes
// through the real handler and blob store, and the stored hash must match the hash the
// generator computed alongside the bytes.
func TestCreatePackStreamsWithoutBuffering(t *testing.T) {
	const bodySize = 64 << 20

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler := packHandler(blobs, &fakePackStore{}, 1<<30)

	gen := newPackGenerator(bodySize)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	req := httptest.NewRequest(http.MethodPost, packsPath+"?name=big_pack", gen)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	runtime.ReadMemStats(&after)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("TotalAlloc grew by %d bytes over the request, want < 16 MiB", grew)
	}

	wantSHA := gen.sha()
	rc, size, err := blobs.Open(wantSHA)
	if err != nil {
		t.Fatalf("Open stored blob %s: %v", wantSHA, err)
	}
	defer rc.Close()
	if size != bodySize+int64(len(packMagic)) {
		t.Errorf("stored size = %d, want %d", size, bodySize+len(packMagic))
	}
	// Reading the blob back is not part of the memory measurement; it only confirms the
	// content is the bytes the generator produced.
	got := sha256.New()
	if _, err := io.Copy(got, rc); err != nil {
		t.Fatalf("hash stored blob: %v", err)
	}
	if hex.EncodeToString(got.Sum(nil)) != wantSHA {
		t.Errorf("stored blob hashes to %s, want %s", hex.EncodeToString(got.Sum(nil)), wantSHA)
	}
}
