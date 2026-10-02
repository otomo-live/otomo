package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/config"
	"github.com/otomo-live/otomo/services/session/migrations"
)

// testDB returns a pool onto the database SESSION_TEST_DATABASE_URL names, with the
// migrations applied. It skips rather than fails when the variable is unset, so a machine
// without Postgres still runs the tests that do not need one.
//
// It refuses a database whose name does not contain "test". That is not politeness: these
// tests create and delete rows, and a typo in one environment variable would otherwise let
// them run against whatever SESSION_DATABASE_URL points at.
func testDB(t *testing.T) *DB {
	t.Helper()

	url := os.Getenv("SESSION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SESSION_TEST_DATABASE_URL is not set; skipping the tests that need Postgres")
	}

	db, err := NewPool(t.Context(), config.Config{DatabaseURL: url, DBMaxConns: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(db.Close)

	var name string
	if err := db.Pool.QueryRow(t.Context(), "select current_database()").Scan(&name); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.Contains(name, "test") {
		t.Fatalf("refusing to run against database %q: SESSION_TEST_DATABASE_URL must name a test database", name)
	}

	migrate(t, url)
	return db
}

// migrate applies the embedded migrations once per process, however many tests call it.
// The result is kept in a package variable rather than reported against the calling test,
// because the goroutine or test that happens to get there first is not the one that should
// carry the failure: every caller checks it and fails with the same message.
var (
	migrateOnce sync.Once
	migrateErr  error
)

func migrate(t *testing.T, url string) {
	t.Helper()

	migrateOnce.Do(func() {
		sqlDB, err := sql.Open("pgx", url)
		if err != nil {
			migrateErr = fmt.Errorf("open postgres for migrations: %w", err)
			return
		}
		defer sqlDB.Close()

		goose.SetBaseFS(migrations.FS)
		if err := goose.SetDialect("postgres"); err != nil {
			migrateErr = fmt.Errorf("set the goose dialect: %w", err)
			return
		}
		if err := goose.Up(sqlDB, "."); err != nil {
			migrateErr = fmt.Errorf("apply migrations: %w", err)
		}
	})

	if migrateErr != nil {
		t.Fatalf("migrations: %v", migrateErr)
	}
}

// newPlayer inserts a profile and returns its id, which most of the schema tests need as
// a foreign key. The name is unique per call so a rerun against a database that was not
// cleaned cannot collide on player_name_uq.
func newPlayer(t *testing.T, db *DB, name string) uuid.UUID {
	t.Helper()

	id := uuid.NewV7()
	_, err := db.Pool.Exec(t.Context(),
		"insert into player_profile (player_id, display_name, discriminator) values ($1, $2, $3)",
		id, name, 1)
	if err != nil {
		t.Fatalf("insert player %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(t.Context(), "delete from player_profile where player_id = $1", id)
	})
	return id
}

// TestWriteAuditRejectsIncompleteEntries is the half of the audit writer that needs no
// database: every field is required, and the cheque happens before any statement is sent,
// so a nil transaction is the right thing to pass here.
func TestWriteAuditRejectsIncompleteEntries(t *testing.T) {
	full := Entry{ActorID: "staff-1", ActorName: "Tanuki", Action: "party.disband_forced", Target: "party-1"}

	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{"no actor id", Entry{ActorName: full.ActorName, Action: full.Action, Target: full.Target}, "no actor_id"},
		{"no actor name", Entry{ActorID: full.ActorID, Action: full.Action, Target: full.Target}, "no actor_name"},
		{"no action", Entry{ActorID: full.ActorID, ActorName: full.ActorName, Target: full.Target}, "no action"},
		{"no target", Entry{ActorID: full.ActorID, ActorName: full.ActorName, Action: full.Action}, "no target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := WriteAudit(t.Context(), nil, tt.entry)
			if err == nil {
				t.Fatal("WriteAudit accepted an incomplete entry")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestNewPoolRejectsAMalformedURL needs no database: a URL pgx cannot parse is rejected
// before any connection is attempted, so a container with a typo dies with a message that
// names the parser rather than hanging on a dial.
func TestNewPoolRejectsAMalformedURL(t *testing.T) {
	for _, bad := range []string{"not a url", "://nope", "postgres://user@host:notaport/db"} {
		t.Run(bad, func(t *testing.T) {
			db, err := NewPool(t.Context(), config.Config{DatabaseURL: bad, DBMaxConns: 1})
			if err == nil {
				db.Close()
				t.Fatalf("NewPool accepted %q", bad)
			}
			if !strings.Contains(err.Error(), "SESSION_DATABASE_URL") {
				t.Errorf("error = %v, want it to name the variable", err)
			}
		})
	}
}

// TestReadyDoesNotCacheACancelledProbe is the store half of the readiness fix: a probe whose
// caller gave up must not be remembered as "Postgres is down" for readyProbeInterval.
func TestReadyDoesNotCacheACancelledProbe(t *testing.T) {
	db := testDB(t)

	// Make the next call ping instead of answering from the cache NewPool primed.
	db.mu.Lock()
	db.lastAt = time.Time{}
	db.mu.Unlock()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.Ready(cancelled); err == nil {
		t.Fatal("Ready with a cancelled context returned nil, want the ping's error")
	}

	if err := db.Ready(t.Context()); err != nil {
		t.Fatalf("Ready after a cancelled probe = %v, want nil: the cancellation was cached", err)
	}
}

// TestMigrationsCreateTheSchema is the check that the embedded files are complete and in
// an order Postgres accepts. A migration that only works on a database where something
// already exists is a deploy that fails on an empty one.
func TestMigrationsCreateTheSchema(t *testing.T) {
	db := testDB(t)

	for _, table := range []string{
		"player_profile", "friendship", "block", "party", "party_member", "party_invite", "audit_log",
	} {
		var exists bool
		err := db.Pool.QueryRow(t.Context(),
			"select exists (select 1 from information_schema.tables where table_name = $1)", table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s is missing", table)
		}
	}

	// Columns added after 00001, each by its own migration.
	for _, col := range []struct{ table, column string }{
		{"player_profile", "name_changed_at"}, // 00002, SE-2
		{"party", "state"},                    // 00003, LB-2
		{"party", "settings"},
		{"party", "state_changed_at"},
		{"party", "allocation_id"},
		{"party_member", "ready"},
		{"party", "match_address"}, // 00004, LB-3
		{"party", "match_port"},
	} {
		var exists bool
		err := db.Pool.QueryRow(t.Context(),
			`select exists (select 1 from information_schema.columns
			                 where table_name = $1 and column_name = $2)`, col.table, col.column).Scan(&exists)
		if err != nil {
			t.Fatalf("look up %s.%s: %v", col.table, col.column, err)
		}
		if !exists {
			t.Errorf("column %s.%s is missing", col.table, col.column)
		}
	}

	// The four indexes the migration header documents as additions. A dropped index is
	// invisible in every functional test and only shows up as a slow query.
	for _, index := range []string{
		"player_name_uq", "friendship_hi_idx", "party_member_party_idx",
		"party_member_joined_idx", "party_invite_expires_idx", "audit_log_at_idx",
		"party_state_idx", // 00003, LB-2
	} {
		var exists bool
		err := db.Pool.QueryRow(t.Context(),
			"select exists (select 1 from pg_indexes where indexname = $1)", index).Scan(&exists)
		if err != nil {
			t.Fatalf("look up index %q: %v", index, err)
		}
		if !exists {
			t.Errorf("index %s is missing", index)
		}
	}
}

// TestMigrationsRunTwiceCleanly is SES-A2's deploy requirement: the Jenkins migration job
// runs before every deploy without knowing whether it already did, so applying the same
// migrations again has to be a no-op rather than an error.
func TestMigrationsRunTwiceCleanly(t *testing.T) {
	url := os.Getenv("SESSION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SESSION_TEST_DATABASE_URL is not set")
	}

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set the goose dialect: %v", err)
	}
	if err := goose.Up(sqlDB, "."); err != nil {
		t.Fatalf("second goose.Up failed: %v", err)
	}
}

// TestWriteAuditLivesInTheCallersTransaction is the property the signature exists for. An
// audit entry written outside the transaction that made the change would survive a
// rollback and record something that never happened — and a wrong record is worse than a
// missing one, because a reader cannot tell it from a right one.
func TestWriteAuditLivesInTheCallersTransaction(t *testing.T) {
	db := testDB(t)
	actor := "staff-" + time.Now().UTC().Format("20060102150405.000000000")

	count := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := db.Pool.QueryRow(t.Context(),
			"select count(*) from audit_log where actor_id = $1", actor).Scan(&n); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		return n
	}

	t.Run("committed", func(t *testing.T) {
		tx, err := db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := WriteAudit(t.Context(), tx, Entry{
			ActorID: actor, ActorName: "Tanuki", Action: "party.disband_forced",
			Target: "party-1", Details: map[string]any{"members": 3, "forced": true},
		}); err != nil {
			tx.Rollback(t.Context())
			t.Fatalf("WriteAudit: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if got := count(t); got != 1 {
			t.Fatalf("audit rows = %d, want 1", got)
		}
	})

	t.Run("rolled back", func(t *testing.T) {
		tx, err := db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := WriteAudit(t.Context(), tx, Entry{
			ActorID: actor, ActorName: "Tanuki", Action: "party.disband_forced", Target: "party-2",
		}); err != nil {
			tx.Rollback(t.Context())
			t.Fatalf("WriteAudit: %v", err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		// The entry from the committed subtest is still there; what must not be is a
		// second one naming party-2.
		var n int
		if err := db.Pool.QueryRow(t.Context(),
			"select count(*) from audit_log where actor_id = $1 and target = $2", actor, "party-2").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("a rolled-back transaction left %d audit rows behind", n)
		}
		if got := count(t); got != 1 {
			t.Errorf("audit rows = %d, want the one committed row", got)
		}
	})
}

// TestWriteAuditStoresDetailsAsJSONB pins the one thing about the storage format that
// callers depend on: an entry with no details is stored as an empty object rather than
// null, so a reader can always index into it.
func TestWriteAuditStoresDetailsAsJSONB(t *testing.T) {
	db := testDB(t)
	actor := "staff-jsonb-" + time.Now().UTC().Format("20060102150405.000000000")

	tx, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(t.Context())

	if err := WriteAudit(t.Context(), tx, Entry{
		ActorID: actor, ActorName: "Tanuki", Action: "party.disband_forced", Target: "party-3",
	}); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}

	var details string
	var notNull bool
	if err := tx.QueryRow(t.Context(),
		"select details::text, details is not null from audit_log where actor_id = $1", actor).
		Scan(&details, &notNull); err != nil {
		t.Fatalf("read details: %v", err)
	}
	if !notNull || details != "{}" {
		t.Errorf("details = %s (not null: %v), want {}", details, notNull)
	}
}

// TestSchemaConstraints pins the invariants the migration states, one subtest each. They
// are checked here rather than only in the handlers that rely on them because a handler
// bug that violated one would otherwise be silent: the constraint is what turns it into a
// failed statement.
func TestSchemaConstraints(t *testing.T) {
	db := testDB(t)

	t.Run("a name is not empty", func(t *testing.T) {
		id := uuid.NewV7()
		_, err := db.Pool.Exec(t.Context(),
			"insert into player_profile (player_id, display_name, discriminator) values ($1, '', 1)", id)
		if err == nil {
			t.Error("an empty display_name was accepted")
		}
	})

	t.Run("a discriminator is in range", func(t *testing.T) {
		for _, d := range []int{0, 10000, -1} {
			id := uuid.NewV7()
			_, err := db.Pool.Exec(t.Context(),
				"insert into player_profile (player_id, display_name, discriminator) values ($1, $2, $3)",
				id, "Tanuki", d)
			if err == nil {
				t.Errorf("discriminator %d was accepted", d)
			}
		}
	})

	t.Run("a name is unique per discriminator, case-folded", func(t *testing.T) {
		// The test database accumulates rows from earlier runs, so the name has to be one
		// this run owns rather than a fixed string.
		name := "tanuki-" + time.Now().UTC().Format("150405.000000000")
		a := newPlayer(t, db, strings.ToLower(name))

		b := uuid.NewV7()
		// Same name, same discriminator: rejected however it is cased, which is what makes
		// "add by the name you see" unambiguous.
		_, err := db.Pool.Exec(t.Context(),
			"insert into player_profile (player_id, display_name, discriminator) values ($1, $2, 1)",
			b, strings.ToUpper(name))
		if err == nil {
			t.Errorf("a case-folded duplicate of %s was accepted alongside %s", name, a)
		}

		// A different discriminator is how a duplicate name is legitimate.
		if _, err := db.Pool.Exec(t.Context(),
			"insert into player_profile (player_id, display_name, discriminator) values ($1, $2, 2)",
			b, name); err != nil {
			t.Errorf("a duplicate name with another discriminator was refused: %v", err)
		} else {
			t.Cleanup(func() {
				_, _ = db.Pool.Exec(t.Context(), "delete from player_profile where player_id = $1", b)
			})
		}
	})

	t.Run("a friendship is stored with the lower id first", func(t *testing.T) {
		lo := newPlayer(t, db, "lo-"+time.Now().UTC().Format("150405.000000000"))
		hi := newPlayer(t, db, "hi-"+time.Now().UTC().Format("150405.000000000"))
		if lo.Compare(hi) > 0 {
			lo, hi = hi, lo
		}

		// The right order is accepted.
		if _, err := db.Pool.Exec(t.Context(),
			"insert into friendship (player_lo, player_hi, state, requested_by) values ($1, $2, 'pending', $1)",
			lo, hi); err != nil {
			t.Fatalf("a correctly ordered friendship was refused: %v", err)
		}
		t.Cleanup(func() {
			_, _ = db.Pool.Exec(t.Context(),
				"delete from friendship where player_lo = $1 and player_hi = $2", lo, hi)
		})

		// The reversed order is refused, which is what stops A→B and B→A from becoming two
		// rows.
		_, err := db.Pool.Exec(t.Context(),
			"insert into friendship (player_lo, player_hi, state, requested_by) values ($1, $2, 'pending', $1)",
			hi, lo)
		if err == nil {
			t.Error("a reversed friendship pair was accepted")
		}

		// The state vocabulary is closed. A second pair is used so this can only fail on
		// the state check rather than on the primary key the insert above already took.
		stateLo := newPlayer(t, db, "state-lo-"+time.Now().UTC().Format("150405.000000000"))
		stateHi := newPlayer(t, db, "state-hi-"+time.Now().UTC().Format("150405.000000000"))
		if stateLo.Compare(stateHi) > 0 {
			stateLo, stateHi = stateHi, stateLo
		}
		_, err = db.Pool.Exec(t.Context(),
			"insert into friendship (player_lo, player_hi, state, requested_by) values ($1, $2, 'blocked', $1)",
			stateLo, stateHi)
		if err == nil {
			t.Error("an unknown friendship state was accepted")
		}
	})

	t.Run("a block is not self-directed", func(t *testing.T) {
		id := newPlayer(t, db, "self-"+time.Now().UTC().Format("150405.000000000"))

		_, err := db.Pool.Exec(t.Context(),
			"insert into block (blocker, blocked) values ($1, $1)", id)
		if err == nil {
			t.Error("a player was allowed to block themselves")
		}
	})

	t.Run("one party per player", func(t *testing.T) {
		id := newPlayer(t, db, "member-"+time.Now().UTC().Format("150405.000000000"))

		first := uuid.NewV7()
		second := uuid.NewV7()
		for _, p := range []uuid.UUID{first, second} {
			if _, err := db.Pool.Exec(t.Context(),
				"insert into party (party_id, leader_id) values ($1, $2)", p, id); err != nil {
				t.Fatalf("insert party: %v", err)
			}
			t.Cleanup(func() {
				_, _ = db.Pool.Exec(t.Context(), "delete from party where party_id = $1", p)
			})
		}

		if _, err := db.Pool.Exec(t.Context(),
			"insert into party_member (player_id, party_id) values ($1, $2)", id, first); err != nil {
			t.Fatalf("join the first party: %v", err)
		}
		// The primary key *is* the rule "one party per player" (SES-D1).
		if _, err := db.Pool.Exec(t.Context(),
			"insert into party_member (player_id, party_id) values ($1, $2)", id, second); err == nil {
			t.Error("a player was allowed into two parties at once")
		}
	})

	t.Run("disbanding a party takes its membership with it", func(t *testing.T) {
		id := newPlayer(t, db, "cascade-"+time.Now().UTC().Format("150405.000000000"))
		partyID := uuid.NewV7()

		if _, err := db.Pool.Exec(t.Context(),
			"insert into party (party_id, leader_id) values ($1, $2)", partyID, id); err != nil {
			t.Fatalf("insert party: %v", err)
		}
		if _, err := db.Pool.Exec(t.Context(),
			"insert into party_member (player_id, party_id) values ($1, $2)", id, partyID); err != nil {
			t.Fatalf("insert member: %v", err)
		}

		// The cascade is what makes a disband one statement rather than a handler that has
		// to remember to empty the party first.
		if _, err := db.Pool.Exec(t.Context(),
			"delete from party where party_id = $1", partyID); err != nil {
			t.Fatalf("delete party: %v", err)
		}

		var members int
		if err := db.Pool.QueryRow(t.Context(),
			"select count(*) from party_member where party_id = $1", partyID).Scan(&members); err != nil {
			t.Fatalf("count members: %v", err)
		}
		if members != 0 {
			t.Errorf("%d members survived their party", members)
		}
	})

	t.Run("one live invite per party and invitee", func(t *testing.T) {
		leader := newPlayer(t, db, "inviter-"+time.Now().UTC().Format("150405.000000000"))
		invitee := newPlayer(t, db, "invitee-"+time.Now().UTC().Format("150405.000000000"))
		partyID := uuid.NewV7()
		if _, err := db.Pool.Exec(t.Context(),
			"insert into party (party_id, leader_id) values ($1, $2)", partyID, leader); err != nil {
			t.Fatalf("insert party: %v", err)
		}
		t.Cleanup(func() {
			_, _ = db.Pool.Exec(t.Context(), "delete from party where party_id = $1", partyID)
		})

		invite := uuid.NewV7()
		if _, err := db.Pool.Exec(t.Context(),
			`insert into party_invite (invite_id, party_id, from_player, to_player, expires_at)
			 values ($1, $2, $3, $4, now() + interval '60 seconds')`,
			invite, partyID, leader, invitee); err != nil {
			t.Fatalf("insert invite: %v", err)
		}

		second := uuid.NewV7()
		_, err := db.Pool.Exec(t.Context(),
			`insert into party_invite (invite_id, party_id, from_player, to_player, expires_at)
			 values ($1, $2, $3, $4, now() + interval '60 seconds')`,
			second, partyID, leader, invitee)
		if err == nil {
			t.Error("a second live invite for the same party and invitee was accepted")
		}
	})
}

// TestReadyCachesItsVerdict pins the rate limit that keeps /readyz from becoming a load on
// Postgres: a load balancer calling it every second must not produce a ping every second —
// and, just as importantly, the cached verdict must expire, or a database that died after
// a successful ping would be reported healthy forever.
//
// The expiry is forced by rewinding lastAt rather than by sleeping out the interval, since
// the interval is ten seconds and a test that waits for it is a test nobody runs.
func TestReadyCachesItsVerdict(t *testing.T) {
	db := testDB(t)

	// A real ping: rewind the clock past the interval so this call cannot be a cache hit.
	db.lastAt = time.Now().Add(-2 * readyProbeInterval)
	if err := db.Ready(t.Context()); err != nil {
		t.Fatalf("Ready on a live database: %v", err)
	}

	// Immediately afterwards the pool is closed. Inside the interval the cached verdict
	// stands, which is what proves no ping is issued per call.
	db.Pool.Close()
	if err := db.Ready(t.Context()); err != nil {
		t.Errorf("Ready pinged again inside the cache interval: %v", err)
	}

	// Once the interval is over the verdict has to be re-derived: a cached "ready" that
	// never expires is worse than no readiness endpoint at all.
	db.lastAt = time.Now().Add(-2 * readyProbeInterval)
	err := db.Ready(t.Context())
	if err == nil {
		t.Fatal("Ready reported a closed pool as ready")
	}
	if !strings.Contains(err.Error(), "postgres unreachable") {
		t.Errorf("error = %v, want it to say postgres is unreachable", err)
	}
}
