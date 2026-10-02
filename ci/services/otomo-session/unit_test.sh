#!/bin/sh
# Pre-deploy tests for session, run by ci/services/Jenkinsfile as the final stages
# of the otomo-session job, one level per stage:
#
#   ci/services/otomo-session/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below; see ci/services/lib/unit_test.sh for the contract
# and what each level is for. Every level is a selection of the service's own Go
# tests, so a level can be reproduced locally with the same `go test`
# line and SESSION_TEST_DATABASE_URL / SESSION_TEST_VALKEY_URL set. go-checks.sh has
# already run the whole suite once; the levels exist so a failure is reported under
# the stage that names what broke.
#
# The Hurl smoke run through the gateway (services/session/smoke/session.hurl) and the
# k6 load script (services/session/load/) need the deployed stack, so they are not
# here; services/session/load/README.md says how to run them.
SERVICE=session
. "$(dirname "$0")/../lib/unit_test.sh"

# gotest <run-pattern> <packages...>: run the matching tests with the race detector,
# never from cache.
gotest() {
  pattern=$1
  shift
  (cd "$SERVICE_DIR" && go test -race -count=1 -run "$pattern" "$@")
}

# need_deps fails the level when the test Postgres or Valkey is missing. Those tests
# skip without them, and a level that passes because everything skipped would be a
# silent hole in the gate.
need_deps() {
  if [ -z "${SESSION_TEST_DATABASE_URL:-}" ]; then
    echo "SESSION_TEST_DATABASE_URL is not set; the $LEVEL level needs the CI Postgres" >&2
    return 1
  fi
  if [ -z "${SESSION_TEST_VALKEY_URL:-}" ]; then
    echo "SESSION_TEST_VALKEY_URL is not set; the $LEVEL level needs the CI Valkey" >&2
    return 1
  fi
}

# The service builds, its config loads, all three listeners start and stop, health and
# readiness answer, and the route table is the spec's.
sanity() {
  (cd "$SERVICE_DIR" && go build ./...) &&
    gotest '^Test(Load|Migrate|HealthAndReadiness|Run|Routes|RoutePatterns|Fallback|RequestID|DefaultsEqualTheSeed|ParseTheSeed)' ./internal/...
}

# Each feature does what its ticket says: profiles (SE-2), the long-poll (SE-4),
# presence (SE-3), friends and blocks (SE-5), parties and the lobby (SE-6, LB-2), the
# rules (LB-1), launch and the return from a match (LB-3, LB-4), and the release check
# (SE-8). The store halves run against Postgres and Valkey.
functional() {
  need_deps &&
    gotest '.' ./internal/api ./internal/launch ./internal/rules ./internal/allocator ./internal/presence &&
    gotest 'Profile|Init|Rename|Party|Invite|Leader|Lobby|Settings|Launch|Return|Game|Friend|Request|Accept|Decline|Remove|List|Find' ./internal/store
}

# The service against its real neighbours: the schema and every constraint in
# Postgres, and the event stream, presence and rate-limit scripts in Valkey, including
# an event produced on one instance waking a poll held on another.
integration() {
  need_deps &&
    gotest '.' ./internal/store ./internal/events ./internal/presence ./internal/ratelimit
}

# The boundaries: the two token domains and staff roles, the internal listener's key,
# the release check on player routes only, blocks, and nothing trusted from a body
# that the contract does not allow.
security() {
  need_deps &&
    gotest '.' ./internal/auth ./internal/servicekey ./internal/server &&
    gotest 'Block|Blocked' ./internal/store &&
    gotest 'Refus|Reject|Invalid|Errors|Mapping|FailsClosed|Spam' ./internal/api
}

# Concurrency: races end in one outcome (one profile, one party per player, one
# launch, one return, one friendship, one heartbeat per window), polls and the hub
# leave no goroutines, and a flood of release mismatches polls Patch once.
scaling() {
  need_deps &&
    gotest 'Concurrent|Simultaneous|Two|Race|Restart|Goroutines|Overfill|WriteOnce|Instance|Refresh|Spam' ./internal/...
}

run_level "$@"
