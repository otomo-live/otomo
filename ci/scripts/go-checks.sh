#!/bin/sh
# The Go stage of CI for one service (COM-12), run inside the `go` container of
# ci/compose.yaml:
#
#   docker compose -f ci/compose.yaml run --rm go ci/scripts/go-checks.sh <service>
#
# Every step runs even after one fails, so a build reports all of its problems at
# once; the script exits 1 if any blocking step failed. golangci-lint is the one
# step that reports without blocking: it has no configuration in this repository
# yet and has never run on this code, so its first findings are a backlog, not a
# regression. Make it blocking by moving it into the `step` list once a
# .golangci.yml lands.
#
# The DB-backed tests run for real here, because ci/compose.yaml sets the
# *_TEST_DATABASE_URL variables that make them skip everywhere else.
set -u

# Pinned rather than @latest, so a new release cannot turn a green commit red.
GOVULNCHECK_VERSION=v1.8.0
GOLANGCI_LINT_VERSION=v2.14.0

svc=${1:-}
root=$(cd "$(dirname "$0")/../.." && pwd)
dir="$root/services/$svc"
if [ -z "$svc" ] || [ ! -f "$dir/go.mod" ]; then
  echo "usage: go-checks.sh <service>, where services/<service>/go.mod exists" >&2
  exit 2
fi
cd "$dir"

failed=""

# step <name> <command...>: run one blocking check and record its result.
step() {
  name=$1
  shift
  echo
  echo "=== $svc: $name ==="
  if "$@"; then
    echo "--- ok: $name"
  else
    echo "--- FAILED: $name"
    failed="$failed $name"
  fi
}

gofmt_clean() {
  unformatted=$(gofmt -l .)
  [ -z "$unformatted" ] && return 0
  echo "not gofmt-formatted:"
  echo "$unformatted"
  return 1
}

# patch owns no schema: its DB tests read Config's, and skip when it is missing.
# Apply Config's migrations to the database PATCH_TEST_DATABASE_URL names first,
# through Config's own migrate command, so that the tests are exercised against
# the schema they will meet in a deployment.
migrate_config_for_patch() {
  (cd "$root/services/config" && CONFIG_DATABASE_URL=$PATCH_TEST_DATABASE_URL go run . migrate)
}

step gofmt gofmt_clean
step "go vet" go vet ./...
if [ "$svc" = patch ]; then
  step "config migrations for patch" migrate_config_for_patch
fi
step "go test -race" go test -race -count=1 ./...
step govulncheck sh -c "go install golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION && govulncheck ./..."
case $svc in
  gateway | gateway_dev) step "shared-package drift" "$root/ci/scripts/shared-package-drift.sh" ;;
esac

echo
echo "=== $svc: golangci-lint (reported, not blocking) ==="
if go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION" && golangci-lint run ./...; then
  echo "--- ok: golangci-lint"
else
  echo "--- golangci-lint reported problems (not blocking; see the header of this script)"
fi

echo
if [ -n "$failed" ]; then
  echo "=== $svc: FAILED:$failed ==="
  exit 1
fi
echo "=== $svc: all blocking checks passed ==="
