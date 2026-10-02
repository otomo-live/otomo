package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/otomo-live/otomo/services/allocator/internal/token"
)

// TestRunGenkeyWritesLoadableKey covers the happy path: run writes a fresh key at -out
// with 0600 permissions, and token.Load — the same function serve uses — accepts it.
func TestRunGenkeyWritesLoadableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.pem")

	if code := run([]string{"genkey", "-out", path}); code != 0 {
		t.Fatalf("run(genkey -out %s) = %d, want 0", path, code)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
	if _, err := token.Load(path); err != nil {
		t.Errorf("token.Load(%s) returned %v", path, err)
	}
}

// TestRunGenkeyRefusesToOverwrite checks that a second run against the same path fails
// and leaves the first key byte-for-byte untouched.
func TestRunGenkeyRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.pem")

	if code := run([]string{"genkey", "-out", path}); code != 0 {
		t.Fatalf("first run(genkey -out %s) = %d, want 0", path, code)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if code := run([]string{"genkey", "-out", path}); code != 1 {
		t.Fatalf("second run(genkey -out %s) = %d, want 1", path, code)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the refused run changed the key file")
	}
}

// TestRunUnknownCommand checks that an unrecognised subcommand prints usage and exits 2.
func TestRunUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus"}); code != 2 {
		t.Errorf("run(bogus) = %d, want 2", code)
	}
}

// TestRunGenkeyDefaultsToEnvPath covers the no -out case: the flag default comes from
// ALLOCATOR_SIGNING_KEY_PATH.
func TestRunGenkeyDefaultsToEnvPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.pem")
	t.Setenv("ALLOCATOR_SIGNING_KEY_PATH", path)

	if code := run([]string{"genkey"}); code != 0 {
		t.Fatalf("run(genkey) = %d, want 0", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat %s: %v", path, err)
	}
}
