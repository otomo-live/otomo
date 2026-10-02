#!/bin/sh
# Pre-deploy tests for matchmaker, run by ci/services/Jenkinsfile as the final stages
# of the otomo-matchmaker job, one level per stage:
#
#   ci/services/otomo-matchmaker/unit_test.sh <sanity|functional|integration|security|scaling>
#
# Each level is a function below. Replace a level's not_implemented with real
# checks; see ci/services/lib/unit_test.sh for the contract and what each level
# is for.
SERVICE=matchmaker
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
