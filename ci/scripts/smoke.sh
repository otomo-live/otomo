#!/bin/sh
# The smoke stage of CI for one service, run inside the `go` container of
# ci/compose.yaml:
#
#   docker compose -f ci/compose.yaml run --rm go ci/scripts/smoke.sh <service>
#
# Each smoke script builds the service from the working tree and exits non-zero
# when a check fails, which is what fails the Jenkins stage.
#
# Only the services below have a smoke test that can run unattended. The auth and
# config scripts are not here: both drive `docker run` against a hand-made
# `otomo-pg` container, an `otomo-test` network and a pre-built `:skeleton`
# image, and neither sets an exit code. Automating them means rewriting them the
# way session/smoke.sh is written, which is its own ticket.
set -u

svc=${1:-}
root=$(cd "$(dirname "$0")/../.." && pwd)

case $svc in
  gateway | gateway_dev)
    exec bash "$root/services/$svc/testdata/smoke/smoke.sh"
    ;;
  session)
    # SESSION_DATABASE_URL and SESSION_VALKEY_URL come from ci/compose.yaml. The
    # script is run from its own directory, where it expects to be.
    cd "$root/services/session" && exec sh smoke.sh
    ;;
  *)
    echo "no automated smoke test for $svc; skipping"
    ;;
esac
