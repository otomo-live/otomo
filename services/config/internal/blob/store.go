// Package blob is the service's content-addressed storage: bytes in, SHA-256 out, and
// the same bytes back again under that hash.
//
// Content addressing is what makes the publish path safe to retry and cheap to cache
// (design/02-config.md §1.2): identical content is stored once, and a changed file gets
// a new name, so no cache anywhere can serve a stale one. Nothing here ever deletes a
// blob — garbage collection is deliberately out of scope for M1 (§8).
//
// M1 stores blobs on a volume shared with Patch's nginx. The interface below is the
// part worth keeping when that becomes an S3-compatible object store; only this
// package changes.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// blobsDir and tmpDir are the two directories under the configured root. tmp
	// must be on the same filesystem as blobs, because the commit step is a rename.
	blobsDir = "blobs"
	tmpDir   = "tmp"

	// TempMaxAge is how old a file in tmp/ must be before SweepTemps removes it.
	// An hour is far longer than any upload of the 512 MB §7 permits, so nothing
	// still being written can be swept.
	TempMaxAge = time.Hour

	// dirMode is owner-writable and world-readable: Patch's nginx runs as a
	// different uid and must be able to read what this service wrote.
	dirMode fs.FileMode = 0o755

	sha256HexLen = 64
)

// ErrNotFound is returned by Open for a hash that is not stored. It wraps
// fs.ErrNotExist, so a caller can test for either.
var ErrNotFound = fmt.Errorf("blob not found: %w", fs.ErrNotExist)

// ErrInvalidHash is returned for a hash that is not 64 lowercase hex characters. It
// is a security boundary, not a tidiness check: the hash becomes a path component, so
// anything else — a slash, a "..", an empty string — would escape the root.
var ErrInvalidHash = errors.New("not a lowercase sha256 hex digest")

// Ref identifies stored content. Size is the byte length of the content the hash was
// computed over, so a caller can record it without re-reading the blob.
type Ref struct {
	SHA256 string
	Size   int64
}

// Store is a content-addressed store rooted at one directory. It is safe for
// concurrent use: every operation is independent, and the one place two calls can
// meet — the rename onto a final path — is atomic and idempotent.
type Store struct {
	root  string
	blobs string
	tmp   string
}

// NewStore prepares root and verifies it is writable.
//
// The writability probe is the point of doing this at start-up: a blob volume mounted
// read-only, or not mounted at all, is a deployment mistake, and the worst possible
// time to discover it is halfway through a publish with a release row about to commit.
// Failing here instead makes the container refuse to start, which is a problem
// somebody notices.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("blob: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve %q: %w", root, err)
	}

	s := &Store{
		root:  abs,
		blobs: filepath.Join(abs, blobsDir),
		tmp:   filepath.Join(abs, tmpDir),
	}
	for _, dir := range []string{s.blobs, s.tmp} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, fmt.Errorf("blob: create %s: %w", dir, err)
		}
	}

	probe, err := os.CreateTemp(s.tmp, "probe-*")
	if err != nil {
		return nil, fmt.Errorf("blob: %s is not writable: %w", s.tmp, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return nil, fmt.Errorf("blob: %s is not writable: %w", s.tmp, err)
	}
	if err := os.Remove(name); err != nil {
		return nil, fmt.Errorf("blob: %s is not writable: %w", s.tmp, err)
	}

	return s, nil
}

// Root returns the absolute directory the store was built on.
func (s *Store) Root() string {
	return s.root
}

// Ready reports whether the blob directories are still there and still a directory.
// It is a stat, cheap enough to call from a readiness probe per request; it does not
// re-test writability, which NewStore proved once at start-up and which a read-only
// remount would not change back.
func (s *Store) Ready() error {
	info, err := os.Stat(s.blobs)
	if err != nil {
		return fmt.Errorf("blob: blobs directory unusable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("blob: %s is not a directory", s.blobs)
	}
	return nil
}

// Put streams r into the store and returns the hash of what it read and how many
// bytes that was. The content is never held in memory: a 512 MB pack costs the same
// resident memory as a 512-byte config document.
//
// Storing the same bytes twice is one blob. The write goes to a temp file, is synced,
// and is then renamed onto its final path — so a crash at any point leaves either no
// blob or the complete one, and never a partial file at a hash that claims to
// describe it.
func (s *Store) Put(ctx context.Context, r io.Reader) (Ref, error) {
	tmp, err := os.CreateTemp(s.tmp, "upload-*")
	if err != nil {
		return Ref{}, fmt.Errorf("blob: create temp file: %w", err)
	}
	name := tmp.Name()

	// The temp file is the only thing this function can leave behind, and it is
	// never the destination: an interrupted upload leaves a stray file in tmp/,
	// never a partial blob under a real hash. Removing it here covers every error
	// and cancellation path; after a successful rename there is nothing left to
	// remove, and the error is ignored.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), &contextReader{ctx: ctx, r: r})
	if err != nil {
		return Ref{}, fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return Ref{}, fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Ref{}, fmt.Errorf("blob: close: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	dst := s.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return Ref{}, fmt.Errorf("blob: create fan-out directory: %w", err)
	}
	// Rename is atomic within a filesystem, which is why tmp/ lives under the same
	// root as blobs/. Two concurrent uploads of identical bytes both rename onto the
	// same path with identical content, so the race is harmless.
	if err := os.Rename(name, dst); err != nil {
		return Ref{}, fmt.Errorf("blob: commit: %w", err)
	}

	return Ref{SHA256: sum, Size: size}, nil
}

// PutBytes is Put for content already in memory, which is the shape the publish path
// needs for a canonicalized config document.
func (s *Store) PutBytes(ctx context.Context, b []byte) (Ref, error) {
	return s.Put(ctx, bytes.NewReader(b))
}

// Exists reports whether content with this hash is stored.
func (s *Store) Exists(sha string) (bool, error) {
	path, err := s.validPath(sha)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("blob: stat %s: %w", sha, err)
	}
	return true, nil
}

// Open returns the stored content and its size. The caller closes the reader.
func (s *Store) Open(sha string) (io.ReadCloser, int64, error) {
	path, err := s.validPath(sha)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("blob %s: %w", sha, ErrNotFound)
		}
		return nil, 0, fmt.Errorf("blob: open %s: %w", sha, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("blob: stat %s: %w", sha, err)
	}
	return f, info.Size(), nil
}

// SweepTemps removes leftover upload temp files older than maxAge and returns how
// many it removed.
//
// It exists because the deferred cleanup in Put cannot run if the process is killed:
// a container that is OOM-killed or SIGKILLed mid-upload leaves its temp file behind,
// and without this a service that gets killed often would slowly fill the volume. Run
// it once at start-up. A file a live upload is still writing is safe — it is younger
// than TempMaxAge by a wide margin.
func (s *Store) SweepTemps(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		return 0, fmt.Errorf("blob: read %s: %w", s.tmp, err)
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0
	var failed []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			failed = append(failed, err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.tmp, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(failed...)
}

// path returns the storage path for a hash, fanning out on its first two byte pairs:
// blobs/ab/cd/abcd…. Two levels keep any one directory to a manageable number of
// entries, which matters on a volume that will hold every pack ever uploaded.
func (s *Store) path(sha string) string {
	return filepath.Join(s.blobs, sha[0:2], sha[2:4], sha)
}

// validPath validates a caller-supplied hash and returns its storage path.
func (s *Store) validPath(sha string) (string, error) {
	if err := validateHash(sha); err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidHash, sha)
	}
	return s.path(sha), nil
}

// validateHash accepts exactly what hex.EncodeToString(sha256.Sum256(...)) produces:
// 64 characters, all lowercase hex. Anything else is rejected rather than normalised,
// so there is exactly one name for any piece of content and no two spellings can
// disagree about which blob they mean.
func validateHash(sha string) error {
	if len(sha) != sha256HexLen {
		return ErrInvalidHash
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return ErrInvalidHash
		}
	}
	return nil
}

// contextReader makes a plain io.Reader honour a context, so cancelling a request
// aborts an upload that io.Copy would otherwise run to completion.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
