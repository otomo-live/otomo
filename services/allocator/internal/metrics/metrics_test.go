package metrics

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/otomo-live/otomo/services/allocator/internal/testdb"
)

// insertServer writes a game_server row directly. The tests are about what the scraped
// gauge reports, not about registration, so they build the pool with SQL.
func insertServer(t *testing.T, db *pgxpool.Pool, serverID, state string) {
	t.Helper()
	if _, err := db.Exec(t.Context(), `
		INSERT INTO game_server (server_id, internal_addr, capacity, state)
		VALUES ($1, '10.0.0.1:27015', 8, $2)`, serverID, state); err != nil {
		t.Fatalf("insert game server %q in state %q: %v", serverID, state, err)
	}
}

// TestServerStatesGauge checks the collector's contract: one query, all four states
// emitted, and a state with no servers reported as 0 rather than omitted. The expected
// text pins the help and type as well as the values.
func TestServerStatesGauge(t *testing.T) {
	db := testdb.Open(t)
	insertServer(t, db, "gs-free", "free")
	insertServer(t, db, "gs-reserved", "reserved")
	insertServer(t, db, "gs-busy", "busy")
	insertServer(t, db, "gs-dead-1", "dead")
	insertServer(t, db, "gs-dead-2", "dead")

	reg := prometheus.NewRegistry()
	New(reg, db)

	testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP allocator_servers Game servers in the pool, by state.
# TYPE allocator_servers gauge
allocator_servers{state="busy"} 1
allocator_servers{state="dead"} 2
allocator_servers{state="free"} 1
allocator_servers{state="reserved"} 1
`), "allocator_servers")
}

// TestLabelsArePreinitialised checks that every counter label combination exists at 0
// before any event, and that the gauge reports an empty pool as four zeroes rather
// than an absent family.
func TestLabelsArePreinitialised(t *testing.T) {
	db := testdb.Open(t)

	reg := prometheus.NewRegistry()
	New(reg, db)

	testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP allocator_allocations_total Allocations requested, by result.
# TYPE allocator_allocations_total counter
allocator_allocations_total{result="conflict"} 0
allocator_allocations_total{result="created"} 0
allocator_allocations_total{result="error"} 0
allocator_allocations_total{result="existing"} 0
allocator_allocations_total{result="invalid"} 0
allocator_allocations_total{result="no_capacity"} 0
`), "allocator_allocations_total")

	testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP allocator_reaped_total Allocations ended by the reaper, by kind.
# TYPE allocator_reaped_total counter
allocator_reaped_total{kind="dead"} 0
allocator_reaped_total{kind="expired"} 0
`), "allocator_reaped_total")

	testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP allocator_callbacks_total Session callbacks drained, by result.
# TYPE allocator_callbacks_total counter
allocator_callbacks_total{result="delivered"} 0
allocator_callbacks_total{result="gave_up"} 0
allocator_callbacks_total{result="retry"} 0
`), "allocator_callbacks_total")

	testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP allocator_servers Game servers in the pool, by state.
# TYPE allocator_servers gauge
allocator_servers{state="busy"} 0
allocator_servers{state="dead"} 0
allocator_servers{state="free"} 0
allocator_servers{state="reserved"} 0
`), "allocator_servers")
}
