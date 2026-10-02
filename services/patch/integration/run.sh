#!/usr/bin/env bash
# Runs Patch's Config -> Patch integration test. Called from
# ci/services/otomo-patch/unit_test.sh; expects PATCH_TEST_DATABASE_URL in the
# environment, pointing at the same database Config's tests use.
set -euo pipefail

cd "$(dirname "$0")/.."

: "${PATCH_TEST_DATABASE_URL:?PATCH_TEST_DATABASE_URL must be set}"

exec go test -tags integration -race -count=1 ./integration/...
