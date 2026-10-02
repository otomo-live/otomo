#!/bin/sh
# Pre-deploy tests for auth, run by ci/services/Jenkinsfile as the final stages
# of the otomo-auth job, one level per stage:
#
#   ci/services/otomo-auth/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below; see ci/services/lib/unit_test.sh for the contract
# and what each level is for. Every level is a selection of the service's own Go
# tests, so a level can be reproduced locally with the same `go test`
# line. go-checks.sh has already run the whole suite once; the levels exist so a
# failure is reported under the stage that names what broke.
SERVICE=auth
. "$(dirname "$0")/../lib/unit_test.sh"

# gotest <run-pattern> <packages...>: run the matching tests with the race detector,
# never from cache.
gotest() {
  pattern=$1
  shift
  (cd "$SERVICE_DIR" && go test -race -count=1 -run "$pattern" "$@")
}

# need_db fails the level when the test database is missing. The DB-backed tests
# skip without it, and a level that passes because everything skipped would be a
# silent hole in the gate.
need_db() {
  if [ -z "${AUTH_TEST_DATABASE_URL:-}" ]; then
    echo "AUTH_TEST_DATABASE_URL is not set; the $LEVEL level needs the CI Postgres" >&2
    return 1
  fi
}

# The service builds, its config loads, and both listeners answer: health,
# readiness, the JWKS, and the 404/405 fallbacks.
sanity() {
  (cd "$SERVICE_DIR" && go build ./...) &&
    gotest 'Load|Healthz|Readyz|Internal|Fallback|JWKSHandler|RequestID' ./internal/...
}

# Each endpoint does what its ticket says, end to end against Postgres: device login
# with a stable sub, refresh rotation and logout, the services
# hand-off, and tokens Gateway's own parser accepts.
functional() {
  need_db &&
    gotest 'EndToEnd|JWKSServes|Anonymous|Refresh|Logout|Issue|BuildJWKS|KeyFile|Counted' ./internal/...
}

# The store against the real schema: migrations, signing keys, accounts and refresh
# families.
integration() {
  need_db &&
    gotest '.' ./internal/store
}

# The boundaries: reuse revokes the family, every bad refresh token is one 401, bad
# bodies are 400, secrets never reach logs or responses (AU-6: a known device ID and
# every token searched for in every log line), and only canonical UUID
# subjects, the player audience and the 30 s leeway are accepted.
security() {
  need_db &&
    gotest 'Reuse|Rejects|NonUUID|Leeway|WrongAudience|Opaque|Refuses|Expired|Unknown|BadTokens|ValidationFailed|LogsNeverCarry|CarryNoSecrets|LogsNoSecret' ./internal/...
}

# Concurrency: first-login races make one account, and concurrent refreshes of one
# token rotate it exactly once.
scaling() {
  need_db &&
    gotest 'Concurrent|Race|Losing' ./internal/store
}

run_level "$@"
