#!/bin/bash
# Fails when gateway_dev's copies of gateway's domain-free packages have drifted.
#
# Run from anywhere; it works on the repository this file is in. Called by both
# .github/workflows/ci.yml and ci/scripts/go-checks.sh, so the file list below is
# the one place it is written down.
#
# gateway_dev carries copies of these files rather than importing them, because
# each service is built from its own directory as the Docker context. Each
# service's own tests cannot see the other copy; this can. The files must stay
# byte-identical apart from the module path in their imports. They are the ones
# with no domain knowledge: the Group and Role types, the COM-5 error body, the
# JWKS registry, and the whole token-verification path, including the algorithm
# pin and the order of the issuer/audience checks.
#
# Files that legitimately differ and are therefore not listed: router/dev.go (the
# route table), config/config.go, the entrypoint, and the metrics names (which
# stay gateway_* in both, on purpose: see the comment in obslog/metrics.go).
#
# fixture_test.go is on the list because it is copied verbatim too: it builds the
# two ephemeral issuer domains the cross-domain tests run against, so drift there
# would silently weaken one copy's proof that a player token cannot enter the
# staff gateway (or the reverse).
set -eu

cd "$(dirname "$0")/../.."

status=0
for f in internal/apierr/apierr.go \
         internal/authn/claims.go \
         internal/authn/jwks.go \
         internal/authn/middleware.go \
         internal/authn/fixture_test.go \
         internal/router/route.go \
         internal/proxy/proxy.go \
         internal/server/server.go \
         internal/clientip/clientip.go; do
  if ! diff -u \
      <(sed 's#github.com/otomo-live/otomo/services/gateway_dev/internal#MODULE/internal#g' "services/gateway_dev/$f") \
      <(sed 's#gateway/gateway/internal#MODULE/internal#g' "services/gateway/$f"); then
    # A GitHub Actions annotation; plain text anywhere else.
    echo "::error file=services/gateway_dev/$f::drifted from services/gateway/$f"
    status=1
  fi
done
if [ "$status" -ne 0 ]; then
  echo "Change both copies, or move the package to the shared platform module (COM-13)."
fi
exit $status
