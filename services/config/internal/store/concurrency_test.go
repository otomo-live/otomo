package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// This file holds the store-level concurrency matrix. The simpler single-assertion
// races live beside the operations they cover (drafts_test.go, versions_test.go,
// schemas_test.go, releases_test.go); these tests add the parts those cannot express
// without a start barrier and a distinct per-writer payload: which writer won, that a
// chain of read-modify-write saves loses no update, that a version can never capture a
// body the draft did not hold at the revision it named, and that a failed audit write
// rolls the change back.

// testSeq disambiguates the DDL names the audit-blocker tests create when two installs
// happen inside the same clock tick.
var testSeq atomic.Int64

// uniqueTestID returns a name safe to use as a SQL identifier and as part of a string
// literal.
func uniqueTestID() string {
	return fmt.Sprintf("%d_%d", time.Now().UnixNano(), testSeq.Add(1))
}

// startGate returns a WaitGroup the racers mark done once they are about to block, a
// channel the test closes to release them, and a WaitGroup the test waits on so it does
// not read a racer's result before the racer has written it. Without the gate the
// goroutines tend to be scheduled one at a time, which is exactly the serialisation the
// tests are trying to prove the database imposes anyway.
func startGate(n int) (*sync.WaitGroup, chan struct{}, *sync.WaitGroup) {
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	return &ready, make(chan struct{}), &done
}

// TestConcurrentDraftSavesHaveOneWinner is the store half of requirement 1: eight saves
// at the same revision must leave exactly one winner, and the draft must hold exactly
// that writer's body. The bodies differ, so the revision alone cannot be the assertion.
func TestConcurrentDraftSavesHaveOneWinner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.conc_draft")

	const writers = 8
	body := func(i int) []byte { return []byte(fmt.Sprintf(`{"writer":%d}`, i)) }

	ready, start, done := startGate(writers)
	saved := make([]store.DraftSaved, writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			defer done.Done()
			saved[i], errs[i] = db.SaveDraft(ctx, name, body(i), 1, actor)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var wins, stales int
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			winner = i
		case errors.Is(err, store.ErrStaleRevision):
			stales++
		default:
			t.Errorf("concurrent SaveDraft %d: %v", i, err)
		}
	}
	if wins != 1 || stales != writers-1 {
		t.Fatalf("outcomes = %d wins / %d stale, want 1 / %d", wins, stales, writers-1)
	}
	if saved[winner].Revision != 2 {
		t.Errorf("winner revision = %d, want 2", saved[winner].Revision)
	}

	final, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft after the race: %v", err)
	}
	if final.Revision != 2 {
		t.Errorf("final revision = %d, want 2", final.Revision)
	}
	var got, want any
	if err := json.Unmarshal(final.Body, &got); err != nil {
		t.Fatalf("final body is not JSON: %v (%s)", err, final.Body)
	}
	if err := json.Unmarshal(body(winner), &want); err != nil {
		t.Fatalf("winner body is not JSON: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("surviving body = %s, want the winner's %s", final.Body, body(winner))
	}
	if n := countDraftAudit(t, db, ctx, name); n != 1 {
		t.Errorf("%d draft.save audit rows after the race, want 1", n)
	}
}

// TestChainedDraftSavesHaveNoLostUpdates hammers the optimistic lock from a different
// angle: each writer loops read-modify-write K times and retries on stale. If two saves
// could both commit from one revision, or a committed save could be silently dropped,
// the final array would hold fewer than N*K entries even though the revision advanced.
func TestChainedDraftSavesHaveNoLostUpdates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	name, actor := createTestNamespace(t, db, "test.conc_chain")

	const (
		writers = 8
		rounds  = 5
	)

	type chainDoc struct {
		Writers []string `json:"writers"`
	}

	ready, start, done := startGate(writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			defer done.Done()

			id := fmt.Sprintf("w%d", i)
			for r := 0; r < rounds; r++ {
				for attempt := 0; ; attempt++ {
					if attempt > 10000 {
						errs[i] = errors.New("gave up retrying after 10000 stale saves")
						return
					}
					d, err := db.Draft(ctx, name)
					if err != nil {
						errs[i] = err
						return
					}
					var doc chainDoc
					if len(d.Body) > 0 {
						if err := json.Unmarshal(d.Body, &doc); err != nil {
							errs[i] = err
							return
						}
					}
					doc.Writers = append(doc.Writers, id)
					body, err := json.Marshal(doc)
					if err != nil {
						errs[i] = err
						return
					}
					if _, err := db.SaveDraft(ctx, name, body, d.Revision, actor); err == nil {
						break
					} else if !errors.Is(err, store.ErrStaleRevision) {
						errs[i] = err
						return
					}
				}
			}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	final, err := db.Draft(ctx, name)
	if err != nil {
		t.Fatalf("Draft after the chain: %v", err)
	}
	wantRevision := 1 + writers*rounds
	if final.Revision != wantRevision {
		t.Errorf("final revision = %d, want %d", final.Revision, wantRevision)
	}
	var doc chainDoc
	if err := json.Unmarshal(final.Body, &doc); err != nil {
		t.Fatalf("final body is not JSON: %v (%s)", err, final.Body)
	}
	if len(doc.Writers) != writers*rounds {
		t.Fatalf("final writers = %d, want %d (a lost update)", len(doc.Writers), writers*rounds)
	}
	counts := make(map[string]int, writers)
	for _, id := range doc.Writers {
		counts[id]++
	}
	for i := 0; i < writers; i++ {
		if id := fmt.Sprintf("w%d", i); counts[id] != rounds {
			t.Errorf("writer %s appended %d times, want %d", id, counts[id], rounds)
		}
	}
}

// TestVersionWhileEditingRace is requirement 4: a version ticket and a draft save
// contend for the same revision. Either the version is cut from the body revision r
// held and the save moves the draft afterwards, or the save moves the draft first and
// the ticket is refused as stale. A version whose bytes are neither of those would mean
// the ticket read a revision it did not own.
func TestVersionWhileEditingRace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const rounds = 20
	for round := 0; round < rounds; round++ {
		name, actor := createTestNamespace(t, db, "test.version_edit")
		fromRevision := []byte(fmt.Sprintf(`{"round":%d,"edit":"r"}`, round))
		if _, err := db.SaveDraft(ctx, name, fromRevision, 1, actor); err != nil {
			t.Fatalf("round %d: SaveDraft: %v", round, err)
		}
		wantCanonical, err := schema.Canonical(fromRevision)
		if err != nil {
			t.Fatalf("round %d: Canonical: %v", round, err)
		}

		var (
			ready     sync.WaitGroup
			done      sync.WaitGroup
			start     = make(chan struct{})
			created   store.Version
			createErr error
			saved     store.DraftSaved
			saveErr   error
			editBody  = []byte(fmt.Sprintf(`{"round":%d,"edit":"b"}`, round))
		)
		ready.Add(2)
		done.Add(2)
		go func() {
			ready.Done()
			<-start
			defer done.Done()
			created, createErr = db.CreateVersion(ctx, name, 2, "cut", actor, variantPrepare(blobs))
		}()
		go func() {
			ready.Done()
			<-start
			defer done.Done()
			saved, saveErr = db.SaveDraft(ctx, name, editBody, 2, actor)
		}()
		ready.Wait()
		close(start)
		done.Wait()

		if saveErr != nil {
			t.Fatalf("round %d: concurrent SaveDraft = %v, want success", round, saveErr)
		}
		if saved.Revision != 3 {
			t.Errorf("round %d: save revision = %d, want 3", round, saved.Revision)
		}

		switch {
		case createErr == nil:
			got, err := db.GetVersion(ctx, name, created.Version)
			if err != nil {
				t.Fatalf("round %d: GetVersion: %v", round, err)
			}
			gotCanonical, err := schema.Canonical(got.Body)
			if err != nil {
				t.Fatalf("round %d: Canonical(stored): %v", round, err)
			}
			if string(gotCanonical) != string(wantCanonical) {
				t.Errorf("round %d: version body = %s, want revision 2's body %s",
					round, gotCanonical, wantCanonical)
			}
		case errors.Is(createErr, store.ErrStaleRevision):
			if n := countVersions(t, db, name); n != 0 {
				t.Errorf("round %d: %d versions after a stale ticket, want 0", round, n)
			}
		default:
			t.Fatalf("round %d: CreateVersion = %v, want success or ErrStaleRevision", round, createErr)
		}
	}
}

// installAuditBlocker creates, for the duration of one test, a trigger that makes any
// audit insert carrying target fail. It is how the "no change without its audit row"
// guarantee is tested without a second database: if the audit write is rejected, the
// whole operation must roll back.
func installAuditBlocker(t *testing.T, db *store.DB, target string) {
	t.Helper()

	id := uniqueTestID()
	fn := "test_audit_block_fn_" + id
	trg := "test_audit_block_trg_" + id
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $block$
		BEGIN
		    IF NEW.target = %s THEN
		        RAISE EXCEPTION 'audit write blocked by test';
		    END IF;
		    RETURN NEW;
		END
		$block$ LANGUAGE plpgsql`, fn, sqlStringLiteral(target))); err != nil {
		t.Fatalf("create audit blocker function: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %s()`, trg, fn)); err != nil {
		t.Fatalf("create audit blocker trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON audit_log`, trg))
		_, _ = db.Pool.Exec(context.Background(),
			fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// sqlStringLiteral quotes s for direct interpolation into a statement. The tests only
// ever pass generated namespace names and the literal "dev", but doubling quotes keeps
// the helper honest.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// TestAuditFailureRollsBackTheChange is requirement 6: for every write path that pairs
// a change with an audit row in one transaction, a failing audit insert must leave no
// trace of the change. The trigger targets the operation's own audit target, derived
// from the store code: the namespace for draft/version/schema/namespace and the channel
// for publish.
func TestAuditFailureRollsBackTheChange(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("draft save", func(t *testing.T) {
		name, actor := createTestNamespace(t, db, "test.audit_draft")
		installAuditBlocker(t, db, name)

		if _, err := db.SaveDraft(ctx, name, []byte(`{"level":9}`), 1, actor); err == nil {
			t.Fatal("SaveDraft succeeded with a failing audit insert")
		}
		d, err := db.Draft(ctx, name)
		if err != nil {
			t.Fatalf("Draft: %v", err)
		}
		if d.Revision != 1 || string(d.Body) != "{}" {
			t.Errorf("draft after the failed save = r%d %s, want r1 {}", d.Revision, d.Body)
		}
		if n := countDraftAudit(t, db, ctx, name); n != 0 {
			t.Errorf("%d draft.save audit rows after the failed save, want 0", n)
		}
	})

	t.Run("create version", func(t *testing.T) {
		name, actor := createTestNamespace(t, db, "test.audit_version")
		if _, err := db.SaveDraft(ctx, name, []byte(`{"level":1}`), 1, actor); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		blobs, err := blob.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		installAuditBlocker(t, db, name)

		if _, err := db.CreateVersion(ctx, name, 2, "cut", actor, variantPrepare(blobs)); err == nil {
			t.Fatal("CreateVersion succeeded with a failing audit insert")
		}
		if n := countVersions(t, db, name); n != 0 {
			t.Errorf("%d version rows after the failed create, want 0", n)
		}
		if n := countVersionAudit(t, db, name); n != 0 {
			t.Errorf("%d version.create audit rows after the failed create, want 0", n)
		}
	})

	t.Run("schema replace", func(t *testing.T) {
		name, actor := createTestNamespace(t, db, "test.audit_schema")
		installAuditBlocker(t, db, name)

		if _, err := db.ReplaceSchema(ctx, name, []byte(`{"type":"object"}`), nil, actor); err == nil {
			t.Fatal("ReplaceSchema succeeded with a failing audit insert")
		}
		var schemas int
		if err := db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM config_schema WHERE namespace = $1`, name).Scan(&schemas); err != nil {
			t.Fatalf("count schemas: %v", err)
		}
		if schemas != 1 {
			t.Errorf("%d schema rows after the failed replace, want the initial 1", schemas)
		}
	})

	t.Run("namespace create", func(t *testing.T) {
		name := uniqueName("test.audit_ns")
		actor := store.Entry{ActorID: uniqueName("staff"), ActorName: "Audit Tester"}
		installAuditBlocker(t, db, name)

		if _, err := db.CreateNamespace(ctx, name, "client", "", actor); err == nil {
			t.Fatal("CreateNamespace succeeded with a failing audit insert")
		}
		var namespaces int
		if err := db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM config_namespace WHERE name = $1`, name).Scan(&namespaces); err != nil {
			t.Fatalf("count namespaces: %v", err)
		}
		if namespaces != 0 {
			t.Errorf("%d namespace rows after the failed create, want 0", namespaces)
		}
	})

	t.Run("publish", func(t *testing.T) {
		lockChannel(t, db, "dev")
		original := channelHead(t, db, "dev")
		t.Cleanup(func() { restoreChannelHead(t, db, "dev", original) })

		blobs, err := blob.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		name, actor, version := publishFixture(t, db, blobs, "test.audit_publish")
		before := countChannelReleases(t, db, "dev")
		installAuditBlocker(t, db, "dev")

		if _, err := db.Publish(ctx, "dev", store.PublishRequest{
			BaseReleaseID:    original,
			Versions:         []store.PublishVersion{{Namespace: name, Version: version}},
			MinClientVersion: "1.0.0",
			Message:          "blocked",
		}, actor, blobs); err == nil {
			t.Fatal("Publish succeeded with a failing audit insert")
		}
		if got := channelHead(t, db, "dev"); got != original {
			t.Errorf("dev head = %d after the failed publish, want %d", got, original)
		}
		if got := countChannelReleases(t, db, "dev"); got != before {
			t.Errorf("dev release count = %d after the failed publish, want %d", got, before)
		}
	})
}
