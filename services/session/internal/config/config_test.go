package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sessionVars is every variable Load reads. Tests clear all of them first, so a
// developer's own SESSION_* environment cannot make a case pass or fail.
var sessionVars = []string{
	"SESSION_LISTEN_ADDR",
	"SESSION_METRICS_ADDR",
	"SESSION_INTERNAL_ADDR",
	"SESSION_CALLBACK_KEY_PATH",
	"SESSION_REQUIRE_RELEASE_HEADER",
	"SESSION_DATABASE_URL",
	"SESSION_DB_MAX_CONNS",
	"SESSION_VALKEY_URL",
	"SESSION_PLAYER_JWKS_URL",
	"SESSION_PLAYER_ISSUER",
	"SESSION_PLAYER_AUDIENCE",
	"SESSION_STAFF_JWKS_URL",
	"SESSION_STAFF_ISSUER",
	"SESSION_STAFF_AUDIENCE",
	"SESSION_JWKS_REFRESH",
	"SESSION_JWT_CLOCK_SKEW",
	"SESSION_EVENT_HOLD",
	"SESSION_READ_TIMEOUT",
	"SESSION_WRITE_TIMEOUT",
	"SESSION_IDLE_TIMEOUT",
	"SESSION_SHUTDOWN_TIMEOUT",
	"SESSION_LOG_LEVEL",
}

// clearEnv blanks every SESSION_* variable. Empty is the same as unset to stringVar,
// which is what makes this sufficient — and it is also the reason a deployment cannot
// blank out a default by exporting an empty string.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range sessionVars {
		t.Setenv(k, "")
	}
}

// serveEnv sets the minimum a serve-mode configuration needs. It is deliberately the
// same values .env.example documents, so a change to one without the other is a test
// failure rather than a drift nobody notices.
func serveEnv(t *testing.T) {
	t.Helper()
	clearEnv(t)
	t.Setenv("SESSION_DATABASE_URL", "postgres://session_rw:pw@postgres:5432/session")
	t.Setenv("SESSION_VALKEY_URL", "valkey://default:pw@valkey:6379/0")
	t.Setenv("SESSION_PLAYER_JWKS_URL", "http://auth/.well-known/jwks.json")
	t.Setenv("SESSION_PLAYER_ISSUER", "https://auth.otomo.internal")
	t.Setenv("SESSION_PLAYER_AUDIENCE", "otomo:player")
	t.Setenv("SESSION_STAFF_JWKS_URL", "http://php-admin/.well-known/jwks.json")
	t.Setenv("SESSION_STAFF_ISSUER", "https://php-admin.otomo.internal")
	t.Setenv("SESSION_STAFF_AUDIENCE", "otomo:staff")
}

func TestLoadAcceptsTheDocumentedServeEnvironment(t *testing.T) {
	serveEnv(t)

	cfg, err := Load(ModeServe)
	if err != nil {
		t.Fatalf("Load(ModeServe) = %v", err)
	}

	// The defaults, which .env.example states and this pins. A default that drifts is a
	// deployment that behaves differently from its documentation.
	for _, tt := range []struct {
		name string
		got  any
		want any
	}{
		{"ListenAddr", cfg.ListenAddr, ":8080"},
		{"MetricsAddr", cfg.MetricsAddr, ":9090"},
		{"InternalAddr", cfg.InternalAddr, ":8081"},
		{"CallbackKeyPath", cfg.CallbackKeyPath, ""},
		{"RequireReleaseHeader", cfg.RequireReleaseHeader, false},
		{"DBMaxConns", cfg.DBMaxConns, int32(8)},
		{"JWKSRefresh", cfg.JWKSRefresh, 30 * time.Second},
		{"JWTClockSkew", cfg.JWTClockSkew, 30 * time.Second},
		{"EventHold", cfg.EventHold, 25 * time.Second},
		{"ReadTimeout", cfg.ReadTimeout, 35 * time.Second},
		{"WriteTimeout", cfg.WriteTimeout, 40 * time.Second},
		{"IdleTimeout", cfg.IdleTimeout, 120 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 15 * time.Second},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestLoadRejectsAnEmptyEnvironment(t *testing.T) {
	clearEnv(t)

	_, err := Load(ModeServe)
	if err == nil {
		t.Fatal("Load(ModeServe) accepted an empty environment")
	}
	// Every missing variable is named, not just the first: one restart should tell an
	// operator everything that is wrong.
	for _, want := range []string{
		"SESSION_DATABASE_URL",
		"SESSION_VALKEY_URL",
		"SESSION_PLAYER_JWKS_URL",
		"SESSION_PLAYER_ISSUER",
		"SESSION_PLAYER_AUDIENCE",
		"SESSION_STAFF_JWKS_URL",
		"SESSION_STAFF_ISSUER",
		"SESSION_STAFF_AUDIENCE",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestLoadReportsEveryProblemAtOnce is the property that makes the error usable: two
// separate mistakes produce one error naming both, rather than one restart each.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	serveEnv(t)
	t.Setenv("SESSION_DB_MAX_CONNS", "many")
	t.Setenv("SESSION_JWKS_REFRESH", "soon")

	_, err := Load(ModeServe)
	if err == nil {
		t.Fatal("Load accepted a malformed environment")
	}
	for _, want := range []string{"SESSION_DB_MAX_CONNS", "SESSION_JWKS_REFRESH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestMigrateNeedsOnlyTheDatabase is the mode asymmetry: migrate runs against a database
// and nothing else, so a deployment job does not have to supply Valkey credentials or
// the issuers' URLs to apply a migration.
func TestMigrateNeedsOnlyTheDatabase(t *testing.T) {
	clearEnv(t)
	t.Setenv("SESSION_DATABASE_URL", "postgres://session_rw:pw@postgres:5432/session")

	if _, err := Load(ModeMigrate); err != nil {
		t.Fatalf("Load(ModeMigrate) = %v", err)
	}

	t.Setenv("SESSION_DATABASE_URL", "")
	if _, err := Load(ModeMigrate); err == nil {
		t.Fatal("Load(ModeMigrate) accepted an environment with no database URL")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"zero connections", "SESSION_DB_MAX_CONNS", "0", "positive integer"},
		{"negative connections", "SESSION_DB_MAX_CONNS", "-1", "positive integer"},
		{"non-numeric connections", "SESSION_DB_MAX_CONNS", "8.5", "positive integer"},
		{"unparseable refresh", "SESSION_JWKS_REFRESH", "30", "not a duration"},
		{"zero refresh", "SESSION_JWKS_REFRESH", "0s", "must be positive"},
		// A zero *timeout* is rejected because net/http reads 0 as "no timeout", which is
		// the opposite of what someone writing 0 means.
		{"zero read timeout", "SESSION_READ_TIMEOUT", "0s", "must be positive"},
		{"negative write timeout", "SESSION_WRITE_TIMEOUT", "-1s", "must be positive"},
		{"negative skew", "SESSION_JWT_CLOCK_SKEW", "-1s", "must not be negative"},
		{"unparseable skew", "SESSION_JWT_CLOCK_SKEW", "30", "not a duration"},
		{"unknown log level", "SESSION_LOG_LEVEL", "verbose", "not a log level"},
		{"not a boolean", "SESSION_REQUIRE_RELEASE_HEADER", "yes", "not true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load(ModeServe)
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestLoadAcceptsAZeroClockSkew is the one variable where zero is a real value rather
// than a typo: containers sharing a clock source need no leeway, and forcing them to
// name a small nonzero value would make them state something they do not mean.
func TestLoadAcceptsAZeroClockSkew(t *testing.T) {
	serveEnv(t)
	t.Setenv("SESSION_JWT_CLOCK_SKEW", "0s")

	cfg, err := Load(ModeServe)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.JWTClockSkew != 0 {
		t.Errorf("JWTClockSkew = %v, want 0", cfg.JWTClockSkew)
	}
}

func TestLoadAcceptsADebuggableLogLevel(t *testing.T) {
	serveEnv(t)
	t.Setenv("SESSION_LOG_LEVEL", "debug")

	cfg, err := Load(ModeServe)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}

func TestLoadRejectsNonAbsoluteJWKSURLs(t *testing.T) {
	for _, bad := range []string{"auth", "/well-known/jwks.json", "://auth/jwks", "auth/.well-known/jwks.json"} {
		t.Run(bad, func(t *testing.T) {
			serveEnv(t)
			t.Setenv("SESSION_PLAYER_JWKS_URL", bad)

			_, err := Load(ModeServe)
			if err == nil {
				t.Fatalf("Load accepted %q as a JWKS URL", bad)
			}
			if !strings.Contains(err.Error(), "not an absolute URL") {
				t.Errorf("error = %v, want it to say the URL is not absolute", err)
			}
		})
	}
}

// TestLoadRejectsOneIssuerForBothDomains is the misconfiguration guard. It is compared on
// the issuer rather than the JWKS URL because the URL check above would already catch a
// duplicated URL only by accident: the dangerous copy-paste gives both domains the same
// issuer while pointing at the same endpoint, and from then on every staff token is a
// valid player token.
func TestLoadRejectsOneIssuerForBothDomains(t *testing.T) {
	serveEnv(t)
	t.Setenv("SESSION_STAFF_ISSUER", "https://auth.otomo.internal")

	_, err := Load(ModeServe)
	if err == nil {
		t.Fatal("Load accepted one issuer for both identity domains")
	}
	if !strings.Contains(err.Error(), "must be distinguishable") {
		t.Errorf("error = %v, want it to explain the two domains must differ", err)
	}
}

// TestLoadRejectsASharedInternalListener: the Allocator's callback must not share a port
// with the public listener, which a gateway routes to, or the metrics one.
func TestLoadRejectsASharedInternalListener(t *testing.T) {
	for _, addr := range []string{":8080", ":9090"} {
		serveEnv(t)
		t.Setenv("SESSION_INTERNAL_ADDR", addr)
		_, err := Load(ModeServe)
		if err == nil || !strings.Contains(err.Error(), "SESSION_INTERNAL_ADDR") {
			t.Errorf("SESSION_INTERNAL_ADDR=%s: err = %v, want it refused", addr, err)
		}
	}
}

// TestLoadRejectsAHoldThatOutlastsTheReadTimeout pins the arithmetic that §4 and §5a
// depend on. A hold at or past the read timeout means the response is written after the
// deadline: the client sees a 504 for every idle poll, and the failure looks like a
// Gateway problem rather than a configuration one.
func TestLoadRejectsAHoldThatOutlastsTheReadTimeout(t *testing.T) {
	tests := []struct {
		name  string
		hold  string
		read  string
		valid bool
	}{
		{"the documented pair", "", "", true},
		{"hold equal to the read timeout", "35s", "35s", false},
		{"hold past the read timeout", "40s", "35s", false},
		{"a hold with room", "20s", "35s", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveEnv(t)
			if tt.hold != "" {
				t.Setenv("SESSION_EVENT_HOLD", tt.hold)
			}
			if tt.read != "" {
				t.Setenv("SESSION_READ_TIMEOUT", tt.read)
			}

			_, err := Load(ModeServe)
			if tt.valid && err != nil {
				t.Fatalf("Load = %v, want it to accept a hold inside the read timeout", err)
			}
			if !tt.valid {
				if err == nil {
					t.Fatal("Load accepted a hold at or past the read timeout")
				}
				if !strings.Contains(err.Error(), "SESSION_EVENT_HOLD") {
					t.Errorf("error = %v, want it to name SESSION_EVENT_HOLD", err)
				}
			}
		})
	}
}

// TestMigrateDoesNotCheckTheHold keeps the two modes honest: a one-shot migration has no
// listener and no long-poll, so it must not refuse an environment over a timeout it will
// never use.
func TestMigrateDoesNotCheckTheHold(t *testing.T) {
	clearEnv(t)
	t.Setenv("SESSION_DATABASE_URL", "postgres://session_rw:pw@postgres:5432/session")
	t.Setenv("SESSION_EVENT_HOLD", "10m")

	if _, err := Load(ModeMigrate); err != nil {
		t.Fatalf("Load(ModeMigrate) = %v", err)
	}
}

// TestLoadIgnoresAnEmptyValueOverwritingADefault pins stringVar's rule, which is what
// clearEnv above relies on: an exported-but-empty variable cannot blank out a default.
func TestLoadIgnoresAnEmptyValueOverwritingADefault(t *testing.T) {
	serveEnv(t)
	t.Setenv("SESSION_LISTEN_ADDR", "")
	t.Setenv("SESSION_LOG_LEVEL", "")

	cfg, err := Load(ModeServe)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want the default", cfg.ListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want the default", cfg.LogLevel)
	}
}
