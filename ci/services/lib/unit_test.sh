# Shared by every ci/services/otomo-<service>/unit_test.sh. Sourced, never run.
#
# The pre-deploy test levels, in the order ci/services/Jenkinsfile runs them.
# They are the last gate before deployment: a level runs only if every level
# before it passed, and a failure in any of them fails the build.
#
#   sanity       - the service starts and answers at all; cheapest, run first
#   functional   - each feature does what its ticket says, end to end
#   integration  - the service against its real neighbours (Postgres, Valkey,
#                  the other services it calls)
#   security     - authn/authz boundaries, input abuse, secrets and headers
#   scaling      - behaviour under load and concurrency
#
# Contract for a level function in a service's unit_test.sh:
#   - it runs from the repository root, inside the service's CI container (the
#     `go` container of ci/compose.yaml for Go services, `node` for adminui), so
#     the toolchain, Postgres, Valkey and every *_TEST_DATABASE_URL are there;
#     $SERVICE and $SERVICE_DIR name the service and its directory
#   - it exits 0 (returns 0) on pass and non-zero on fail
#   - a level with nothing written yet calls `not_implemented`, which passes and
#     says so in the build log, so an empty level is visible rather than silent

LEVELS="sanity functional integration security scaling"

not_implemented() {
  echo "STUB: no $LEVEL tests for $SERVICE yet; passing"
}

# run_level <level>: validate the level, then call the function of that name.
run_level() {
  LEVEL=${1:-}
  case " $LEVELS " in
    *" $LEVEL "*) ;;
    *)
      echo "usage: unit_test.sh <level>, where <level> is one of: $LEVELS" >&2
      exit 2
      ;;
  esac
  root=$(cd "$(dirname "$0")/../../.." && pwd)
  cd "$root" || exit 2
  SERVICE_DIR="$root/services/$SERVICE"
  export SERVICE SERVICE_DIR LEVEL

  echo "=== $SERVICE: $LEVEL ==="
  "$LEVEL"
  status=$?
  if [ "$status" -eq 0 ]; then
    echo "--- ok: $SERVICE $LEVEL"
  else
    echo "--- FAILED: $SERVICE $LEVEL (exit $status)"
  fi
  exit "$status"
}
