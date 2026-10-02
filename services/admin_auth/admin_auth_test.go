package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otomo-live/otomo/services/admin_auth/internal/mfa"
)

// TestGenkeyTOTPWritesAKeyAndRefusesToOverwrite exercises the local-only TOTP key
// command. It must not need a database or signing key, write mode 0600, and leave an
// existing file untouched.
func TestGenkeyTOTPWritesAKeyAndRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin_auth_totp.key")

	if code := run([]string{"genkey", "--totp", path}); code != 0 {
		t.Fatalf("genkey --totp exit code = %d, want 0", code)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := mfa.LoadKey(path); err != nil {
		t.Fatalf("the generated key does not load: %v", err)
	}

	if code := run([]string{"genkey", "--totp", path}); code == 0 {
		t.Fatal("genkey --totp overwrote an existing key file")
	}
}
