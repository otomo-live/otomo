package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// filesUnder returns every regular file below dir, as paths relative to dir. Tests
// assert on this rather than on a specific path where what matters is that *nothing*
// was left behind.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func TestNewStoreRejectsAnEmptyRoot(t *testing.T) {
	if _, err := NewStore("  "); err == nil {
		t.Fatal("NewStore accepted an empty root")
	}
}

// TestNewStoreCreatesAndProbesTheVolume is CFG-A3's start-up half: a blob volume that
// is missing, read-only or not mounted must fail here rather than halfway through a
// publish.
func TestNewStoreCreatesAndProbesTheVolume(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "blobs")

	s, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Root() != root {
		t.Errorf("Root() = %q, want %q", s.Root(), root)
	}
	for _, dir := range []string{"blobs", "tmp"} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
	if err := s.Ready(); err != nil {
		t.Errorf("Ready() = %v", err)
	}
	// The probe must not leave its own file behind.
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("NewStore left %v behind", files)
	}
}

func TestNewStoreFailsOnAReadOnlyVolume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0555 directory is still writable")
	}

	// A volume that is mounted read-only, with nothing in it yet: the failure is in
	// creating the subdirectories.
	t.Run("cannot create the subdirectories", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(root, 0o555); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, dirMode) })

		if _, err := NewStore(root); err == nil {
			t.Fatal("NewStore accepted a read-only volume")
		}
	})

	// The subtler one, and the reason the probe exists at all: the root is writable
	// but the upload directory inside it is not, which MkdirAll cannot detect.
	t.Run("cannot write in tmp", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, tmpDir), dirMode); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Chmod(filepath.Join(root, tmpDir), 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, tmpDir), dirMode) })

		if _, err := NewStore(root); err == nil {
			t.Fatal("NewStore accepted an unwritable tmp directory")
		} else if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("error = %v, want a writability complaint", err)
		}
	})
}

func TestPutStoresContentOnce(t *testing.T) {
	s := newStore(t)
	content := []byte("namespace: gameplay\ndraft:\n  round_time: 90\n")

	first, err := s.PutBytes(t.Context(), content)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := s.PutBytes(t.Context(), content)
	if err != nil {
		t.Fatalf("Put (again): %v", err)
	}

	if first != second {
		t.Errorf("Put returned %+v then %+v for identical content", first, second)
	}
	if first.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", first.Size, len(content))
	}

	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); first.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", first.SHA256, want)
	}

	// The layout is part of the contract: Patch's nginx serves it, so it is asserted
	// here rather than left to the deployment to discover.
	wantRel := filepath.Join(blobsDir, first.SHA256[0:2], first.SHA256[2:4], first.SHA256)
	if files := filesUnder(t, s.root); len(files) != 1 || files[0] != wantRel {
		t.Errorf("stored files = %v, want exactly [%s]", files, wantRel)
	}
}

func TestPutThenOpenRoundTrips(t *testing.T) {
	s := newStore(t)
	content := bytes.Repeat([]byte("otomo"), 4096)

	ref, err := s.PutBytes(t.Context(), content)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, size, err := s.Open(ref.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if size != int64(len(content)) {
		t.Errorf("Open size = %d, want %d", size, len(content))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content differs from what was stored (%d bytes vs %d)", len(got), len(content))
	}

	ok, err := s.Exists(ref.SHA256)
	if err != nil || !ok {
		t.Errorf("Exists = %v, %v; want true, nil", ok, err)
	}
	if files := filesUnder(t, filepath.Join(s.root, tmpDir)); len(files) != 0 {
		t.Errorf("Put left temp files behind: %v", files)
	}
}

func TestOpenAndExistsForAnAbsentHash(t *testing.T) {
	s := newStore(t)
	sha := strings.Repeat("a", sha256HexLen)

	if _, _, err := s.Open(sha); !errors.Is(err, ErrNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open error = %v, want ErrNotFound wrapping fs.ErrNotExist", err)
	}
	if ok, err := s.Exists(sha); ok || err != nil {
		t.Errorf("Exists = %v, %v; want false, nil", ok, err)
	}
}

// TestHashesAreValidatedAsPaths is the security boundary, not a tidiness check: the
// hash becomes a path component, so a traversal or an absolute path here would be a
// file disclosure primitive.
func TestHashesAreValidatedAsPaths(t *testing.T) {
	s := newStore(t)

	bad := []string{
		"",
		"abc",
		strings.Repeat("a", sha256HexLen-1),
		strings.Repeat("a", sha256HexLen+1),
		strings.Repeat("A", sha256HexLen), // uppercase: not what hex.EncodeToString produces
		strings.Repeat("g", sha256HexLen),
		"../../etc/passwd" + strings.Repeat("a", sha256HexLen-16),
		"/etc/passwd" + strings.Repeat("a", sha256HexLen-11),
		"ab/cd" + strings.Repeat("a", sha256HexLen-5),
		strings.Repeat("a", sha256HexLen-1) + " ",
	}
	for _, sha := range bad {
		if _, _, err := s.Open(sha); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Open(%q) error = %v, want ErrInvalidHash", sha, err)
		}
		if _, err := s.Exists(sha); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Exists(%q) error = %v, want ErrInvalidHash", sha, err)
		}
	}
}

// cancellingReader yields some bytes, cancels the context, and then reports EOF. It
// stands in for a client that hangs up mid-upload.
type cancellingReader struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	n := copy(p, []byte("partial content that will never be a blob"))
	r.cancel()
	return n, nil
}

// TestCancelledPutLeavesNothingBehind is the "crash mid-upload" half of CFG-A3,
// observed from the code rather than from a killed process: the temp file is removed,
// and no blob exists — so nothing can ever read a truncated file that claims a hash
// describing the whole of it.
func TestCancelledPutLeavesNothingBehind(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := s.Put(ctx, &cancellingReader{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put error = %v, want context.Canceled", err)
	}
	if files := filesUnder(t, s.root); len(files) != 0 {
		t.Errorf("a cancelled upload left %v behind", files)
	}
	if ok, err := s.Exists(sha256Hex([]byte("partial content that will never be a blob"))); ok || err != nil {
		t.Errorf("Exists = %v, %v; want false, nil", ok, err)
	}
}

// TestSweepTempsRemovesOnlyStaleFiles covers the case Put cannot: a process that was
// SIGKILLed and never ran its deferred cleanup.
func TestSweepTempsRemovesOnlyStaleFiles(t *testing.T) {
	s := newStore(t)

	old := filepath.Join(s.tmp, "upload-old")
	fresh := filepath.Join(s.tmp, "upload-fresh")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	stale := time.Now().Add(-2 * TempMaxAge)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// A directory in tmp/ is not an upload and must be left alone.
	if err := os.Mkdir(filepath.Join(s.tmp, "keep"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	removed, err := s.SweepTemps(TempMaxAge)
	if err != nil {
		t.Fatalf("SweepTemps: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the stale temp file survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh temp file was swept: %v", err)
	}
	if info, err := os.Stat(filepath.Join(s.tmp, "keep")); err != nil || !info.IsDir() {
		t.Errorf("the directory in tmp/ was touched: %v", err)
	}
}

func TestPutIsConcurrencySafe(t *testing.T) {
	s := newStore(t)
	content := []byte(strings.Repeat("same bytes, many writers\n", 64))
	want := sha256Hex(content)

	refs := make(chan Ref, 8)
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			ref, err := s.PutBytes(t.Context(), content)
			if err != nil {
				errs <- err
				return
			}
			refs <- ref
		}()
	}

	for range 8 {
		select {
		case err := <-errs:
			t.Fatalf("concurrent Put: %v", err)
		case ref := <-refs:
			if ref.SHA256 != want {
				t.Errorf("SHA256 = %q, want %q", ref.SHA256, want)
			}
		}
	}
	if files := filesUnder(t, filepath.Join(s.root, blobsDir)); len(files) != 1 {
		t.Errorf("blobs directory holds %d files, want exactly 1", len(files))
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
