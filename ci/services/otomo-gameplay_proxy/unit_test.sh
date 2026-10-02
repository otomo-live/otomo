#!/bin/sh
# Pre-deploy tests for gameplay_proxy, run by ci/services/Jenkinsfile as the final
# stages of the otomo-gameplay_proxy job, one level per stage:
#
#   ci/services/otomo-gameplay_proxy/unit_test.sh <sanity|functional|integration|security|scaling>
#
# See ci/services/lib/unit_test.sh for the contract. The proxy has no database: its
# end-to-end tests run a fake Allocator (httptest) and a UDP echo game server in
# process, so every level runs without the CI Postgres.
SERVICE=gameplay_proxy
. "$(dirname "$0")/../lib/unit_test.sh"

gotest() {
  pattern=$1
  shift
  (cd "$SERVICE_DIR" && go test -race -count=1 -run "$pattern" "$@")
}

# Builds, and the process-local pieces: config and the internal listener.
sanity() {
  (cd "$SERVICE_DIR" && go build ./... && go vet ./...) &&
    gotest '.' ./internal/config
}

# Every package's unit tests.
functional() {
  gotest '.' ./...
}

# Handshake to forwarding over real UDP sockets on loopback, against the fake Allocator.
integration() {
  gotest 'EndToEnd|E2E' ./...
}

# Ticket verification (design/14 §5) and the replay guard.
security() {
  gotest '.' ./internal/ticket
}

scaling() {
  not_implemented
}

run_level "$@"
