#!/bin/sh
# Pre-deploy tests for dashboard, run by ci/services/Jenkinsfile as the final stages
# of the otomo-dashboard job, one level per stage:
#
#   ci/services/otomo-dashboard/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below. Replace a level's not_implemented with real
# checks; see ci/services/lib/unit_test.sh for the contract and what each level
# is for.
SERVICE=dashboard
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
