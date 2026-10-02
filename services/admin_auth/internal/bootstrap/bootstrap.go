// Package bootstrap implements the one-shot bootstrap-root command: it creates the
// single break-glass root staff account from the password secret, or rotates that
// account's password.
//
// The root account is password-only and is the only account that exists before any
// admin does, so its creation cannot go through the normal invite flow. Run is the
// whole command minus flag parsing and config loading, which is what lets the tests
// drive it directly instead of spawning the binary.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/otomo-live/otomo/services/admin_auth/internal/password"
	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
)

// Minimum password length. The root password is stored in a host secret and read by
// an operator over SSH, so it should be long even though nothing types it daily.
const minPasswordLength = 16

// Defaults for the root account when the command's flags are not given.
const (
	DefaultEmail = "root@otomo.internal"
	DefaultName  = "Root"
)

// Options is one bootstrap-root invocation. PasswordFile is the path to the secret;
// Warn receives non-fatal warnings and defaults to os.Stderr.
type Options struct {
	Email        string
	Name         string
	Rotate       bool
	PasswordFile string
	Warn         io.Writer
}

// Run performs one bootstrap-root invocation and returns the human-readable result
// line(s) to print on success. A non-nil error means the command should exit non-zero;
// the error never contains the password.
//
// A missing root is created. An existing root is left alone unless Rotate is set, in
// which case its password is replaced and its live sessions are revoked. Creating
// relies on the single-root unique index rather than the pre-flight read, so two
// concurrent runs cannot both insert: the loser's 23505 is reported as "already
// exists".
func Run(ctx context.Context, db *store.DB, opts Options) (string, error) {
	if opts.Email == "" {
		opts.Email = DefaultEmail
	}
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	warn := opts.Warn
	if warn == nil {
		warn = os.Stderr
	}

	pw, loose, err := ReadPassword(opts.PasswordFile)
	if err != nil {
		return "", err
	}
	if loose {
		fmt.Fprintf(warn, "warning: %s is group- or world-readable; restrict it to 0600\n", opts.PasswordFile)
	}

	existing, err := db.Root(ctx)
	switch {
	case err == nil:
		if !opts.Rotate {
			msg := fmt.Sprintf("root already exists: %s", existing.Email)
			if !strings.EqualFold(opts.Email, existing.Email) {
				msg += fmt.Sprintf("\nnote: keeping the existing root email %s", existing.Email)
			}
			return msg, nil
		}
		hash, err := password.Hash(pw)
		if err != nil {
			return "", err
		}
		revoked, err := db.RotateRoot(ctx, existing.ID, hash)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("root password rotated; %d sessions revoked", revoked), nil
	case errors.Is(err, store.ErrNotFound):
		hash, err := password.Hash(pw)
		if err != nil {
			return "", err
		}
		if _, err := db.CreateRoot(ctx, opts.Email, opts.Name, hash); err != nil {
			if errors.Is(err, store.ErrRootExists) {
				// Another run won the race between the read above and this insert.
				now, reloadErr := db.Root(ctx)
				if reloadErr != nil {
					return "", reloadErr
				}
				return fmt.Sprintf("root already exists: %s", now.Email), nil
			}
			return "", err
		}
		if opts.Rotate {
			return fmt.Sprintf("root created: %s (no root existed to rotate)", strings.ToLower(opts.Email)), nil
		}
		return fmt.Sprintf("root created: %s", strings.ToLower(opts.Email)), nil
	default:
		return "", err
	}
}

// ReadPassword reads and validates the root password from path. Exactly one trailing
// "\n" or "\r\n" is trimmed — a lone "\r" is data, not a line ending — and then the
// password must be at least minPasswordLength bytes. The password itself is never put
// in an error.
//
// loose reports whether the file is group- or world-readable. That is a warning, not
// a failure: the caller decides whether to refuse, and a root break-glass secret that
// is merely too permissive should still work.
func ReadPassword(path string) (pw string, loose bool, err error) {
	if path == "" {
		return "", false, errors.New("bootstrap-root: ADMIN_AUTH_ROOT_PASSWORD_FILE is empty")
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("bootstrap-root: cannot stat the root password file: %w", err)
	}
	loose = info.Mode().Perm()&0o077 != 0

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", loose, fmt.Errorf("bootstrap-root: cannot read the root password file: %w", err)
	}

	switch {
	case len(raw) >= 2 && raw[len(raw)-2] == '\r' && raw[len(raw)-1] == '\n':
		raw = raw[:len(raw)-2]
	case len(raw) >= 1 && raw[len(raw)-1] == '\n':
		raw = raw[:len(raw)-1]
	}

	if len(raw) == 0 {
		return "", loose, errors.New("bootstrap-root: the root password file is empty")
	}
	if len(raw) < minPasswordLength {
		return "", loose, fmt.Errorf("bootstrap-root: the root password must be at least %d characters", minPasswordLength)
	}
	return string(raw), loose, nil
}
