package store

import (
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/allocator/internal/config"
)

// TestNewPoolRejectsMalformedURL exercises the one failure NewPool can reach without a
// Postgres instance: a database URL that does not parse. It is worth a test because
// NewPool is where a typo in ALLOCATOR_DATABASE_URL has to surface, and because the
// error must name the variable an operator has to fix rather than the parser.
func TestNewPoolRejectsMalformedURL(t *testing.T) {
	for _, databaseURL := range []string{
		"postgres://user:pass@host:5432/allocator?sslmode=%zz",
		"postgres://user:pass@host:notaport/allocator",
	} {
		t.Run(databaseURL, func(t *testing.T) {
			_, err := NewPool(t.Context(), config.Config{DatabaseURL: databaseURL, DBMaxConns: 1})
			if err == nil {
				t.Fatalf("NewPool accepted %q", databaseURL)
			}
			if !strings.Contains(err.Error(), "ALLOCATOR_DATABASE_URL") {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}
