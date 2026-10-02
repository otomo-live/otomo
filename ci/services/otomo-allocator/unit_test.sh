#!/bin/sh
# Pre-deploy tests for allocator, run by ci/services/Jenkinsfile as the final stages
# of the otomo-allocator job, one level per stage:
#
#   ci/services/otomo-allocator/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below; see ci/services/lib/unit_test.sh for the contract
# and what each level is for. Levels that need Postgres read
# ALLOCATOR_TEST_DATABASE_URL (ci/compose.yaml) and fail loudly without it.
SERVICE=allocator
. "$(dirname "$0")/../lib/unit_test.sh"

gotest() {
  pattern=$1
  shift
  (cd "$SERVICE_DIR" && go test -race -count=1 -run "$pattern" "$@")
}

need_db() {
  if [ -z "${ALLOCATOR_TEST_DATABASE_URL:-}" ]; then
    echo "ALLOCATOR_TEST_DATABASE_URL is not set; the $LEVEL level needs the CI Postgres" >&2
    return 1
  fi
}

# Builds, and the process-local pieces: config, listeners, health, errors.
sanity() {
  (cd "$SERVICE_DIR" && go build ./... && go vet ./...) &&
    gotest '.' ./internal/config ./internal/server
}

# Every package's unit tests.
functional() {
  gotest '.' ./internal/...
}

# The migrations apply to a real Postgres, twice (goose is idempotent).
integration() {
  need_db &&
    (cd "$SERVICE_DIR" &&
      ALLOCATOR_DATABASE_URL=$ALLOCATOR_TEST_DATABASE_URL go run . migrate &&
      ALLOCATOR_DATABASE_URL=$ALLOCATOR_TEST_DATABASE_URL go run . migrate) &&
    gotest '.' ./internal/store ./internal/pool
}

# Service-key roles (D4) and join-ticket verification.
security() {
  gotest '.' ./internal/servicekey &&
    if [ -d "$SERVICE_DIR/internal/token" ]; then gotest '.' ./internal/token; fi
}

# Concurrent allocation: parties racing for scarce servers never
# share one, and racing retries for one party get one allocation.
scaling() {
  need_db &&
    gotest 'Concurren|Race' ./internal/pool
}

run_level "$@"
