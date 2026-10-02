#!/bin/sh
# Pre-deploy tests for admin_auth, run by ci/services/Jenkinsfile as the final stages
# of the otomo-admin_auth job, one level per stage:
#
#   ci/services/otomo-admin_auth/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below. Replace a level's not_implemented with real
# checks; see ci/services/lib/unit_test.sh for the contract and what each level
# is for.
SERVICE=admin_auth
. "$(dirname "$0")/../lib/unit_test.sh"

sanity() {
  not_implemented
}

functional() {
  not_implemented
}

integration() {
  not_implemented
}

security() {
  not_implemented
}

scaling() {
  not_implemented
}

run_level "$@"
