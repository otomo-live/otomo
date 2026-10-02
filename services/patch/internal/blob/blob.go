// Package blob is a read-only view of the blob volume Config writes.
//
// Patch never writes a blob: it serves the content-addressed files Config stored and
// named in a release manifest. So this package is deliberately only the configured
// root and a readiness stat — the writer lives in Config's internal/blob, not here.
package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// blobsDir is the directory under the root that holds committed blobs. It mirrors
// Config's internal/blob writer exactly: Config stores a blob at
// <root>/blobs/ab/cd/abcd…, fanned out on the first two byte pairs of the digest.
// This constant and path() must stay in lockstep with that writer; there is no
// negotiation or index, the digest is the whole address.
const blobsDir = "blobs"

// sha256HexLen is the length of a lowercase hex SHA-256 digest, the only shape a
// blob name may take.
const sha256HexLen = 64

// ErrInvalidHash is returned for a hash that is not exactly 64 lowercase hex
// characters. It is a security boundary, not tidiness: the hash becomes a path
// component, so a slash, a dot or an uppercase spelling must never reach the
// filesystem. The HTTP handler validates first; this keeps a direct caller from
// indexing past a short string or escaping the root.
var ErrInvalidHash = errors.New("not a lowercase sha256 hex digest")

// ErrNotRegular is returned when the path exists but is not a regular file, such as
// a directory planted at the blob's name. Patch serves files only.
var ErrNotRegular = errors.New("blob is not a regular file")

// Root is the configured blob directory. Patch only ever reads from it, and the
// volume is expected to be mounted read-only into the container.
type Root struct {
	path string
}

// NewRoot verifies at start-up that root exists and is a directory, so a container
// with the volume missing or mounted as a file fails immediately with a specific
// message rather than serving 404s for every blob.
func NewRoot(root string) (*Root, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("blob root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("blob root %s is not a directory", root)
	}
	return &Root{path: root}, nil
}

// Path returns the directory the root was built on.
func (r *Root) Path() string {
	return r.path
}

// Ready reports whether the blob root is still a directory. It is a stat, cheap
// enough to call from a readiness probe per request; it does not re-test
// writability, which Patch never needs and which a read-only mount would reject
// anyway.
func (r *Root) Ready() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("blob root unusable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("blob root %s is not a directory", r.path)
	}
	return nil
}

// Open opens the blob named by a lowercase 64-character hex sha256 digest for
// reading and returns the file together with its FileInfo. The caller closes the
// file. The digest is validated here as well as at the HTTP boundary so a malformed
// value can never become a path component or index past a short string.
//
// The path formula is Config's, byte for byte:
//
//	<root>/blobs/<sha[0:2]>/<sha[2:4]>/<sha>
//
// A missing file, a directory at the name, or any other non-regular file is an
// error; the handler collapses all of them to a 404 because to a client there is
// simply no blob there.
func (r *Root) Open(sha string) (*os.File, fs.FileInfo, error) {
	if err := validateHash(sha); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(filepath.Join(r.path, blobsDir, sha[0:2], sha[2:4], sha))
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("blob: stat %s: %w", sha, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, sha)
	}
	return f, info, nil
}

// validateHash accepts exactly what Config's writer names a blob with: 64
// characters, all lowercase hex. Anything else is rejected rather than normalised,
// so there is exactly one name for any piece of content.
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
