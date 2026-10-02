#!/bin/sh
# Pre-deploy tests for patch, run by ci/services/Jenkinsfile as the final stages
# of the otomo-patch job, one level per stage:
#
#   ci/services/otomo-patch/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below. Replace a level's not_implemented with real
# checks; see ci/services/lib/unit_test.sh for the contract and what each level
# is for.
SERVICE=patch
. "$(dirname "$0")/../lib/unit_test.sh"

sanity() {
  not_implemented
}

functional() {
  not_implemented
}

integration() {
  # Config -> Patch end to end: the real config and patch binaries against the CI
  # Postgres; publish moves the manifest, rollback brings the previous one back
  # byte for byte.
  bash "$SERVICE_DIR/integration/run.sh"
}

security() {
  not_implemented
}

scaling() {
  not_implemented
}

run_level "$@"
