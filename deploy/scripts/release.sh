#!/bin/sh
# COM-8-lite: build one service, tag it with a git SHA, push that tag to the
# registry this stack runs, and move the running container onto it.
#
# This is deliberately not COM-8. COM-8 is the shared Jenkins pipeline: test,
# build, tag, push, migrate, deploy, poll /readyz, roll back on failure. What is
# here is the deploy half, runnable by hand from the host, so that the registry,
# the tag scheme and the readiness probe are all exercised before a pipeline is
# allowed to depend on them.
#
# Usage:
#   deploy/scripts/release.sh <service> [tag]
#
# <service> is one of auth, gateway, gateway_dev. [tag] defaults to the short git
# SHA of the working tree. Nothing here runs tests: COM-12's CI stage does that,
# and a release script that also tested would be a second place for the gate to
# be configured differently.

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

service=${1:-}
if [ -z "$service" ]; then
    echo "usage: release.sh <auth|gateway|gateway_dev> [tag]" >&2
    exit 2
fi

tag=${2:-$(git -C "$DEPLOY_DIR/.." rev-parse --short HEAD 2>/dev/null || true)}
if [ -z "$tag" ]; then
    echo "release.sh: no tag given and no git revision to fall back on; pass one explicitly" >&2
    exit 2
fi
# A git SHA passes this trivially. It is here because the tag is interpolated
# into a sed expression and a Compose variable below, and because Docker rejects
# some characters with an error that names the manifest rather than the input.
case $tag in
    -* | *[!A-Za-z0-9._-]*)
        echo "release.sh: tag '$tag' may only contain letters, digits, dot, underscore and dash" >&2
        exit 2
        ;;
esac

case $service in
    auth)
        tag_var=OTOMO_AUTH_TAG
        # /readyz on auth is a real gate: it turns green only once startup
        # finished and the cached Postgres ping is healthy
        # (services/auth/README.md:120).
        probe_host=auth
        probe_path=/readyz
        ;;
    gateway)
        tag_var=OTOMO_GATEWAY_TAG
        # /healthz, not /readyz. The gateway's /readyz now depends on both token
        # domains, and the staff one is services/admin_auth in this stack, so
        # gating a gateway release on it would couple that release to a second
        # service. Liveness is the honest gate for a single-service deploy; COM-8
        # can decide whether to wait on /readyz instead.
        probe_host=gateway
        probe_path=/healthz
        ;;
    gateway_dev)
        tag_var=OTOMO_GATEWAY_DEV_TAG
        probe_host=gateway_dev
        probe_path=/healthz
        ;;
    *)
        echo "release.sh: '$service' is not a service with an image. Only auth, gateway and gateway_dev are built here." >&2
        exit 2
        ;;
esac

grep -q "^$tag_var=" .env || die "release.sh: $tag_var is not in deploy/.env; regenerate it from deploy/.env.example"

echo "releasing $service as $tag ($tag_var)"

# Exported for this invocation only: the image is built and pushed under the
# release tag, while .env keeps naming whatever the running stack uses until the
# swap below succeeds.
export "$tag_var=$tag"

echo
echo "building"
$COMPOSE build "$service"

echo
echo "pushing to the registry"
$COMPOSE push "$service"

# Written after the push, so a failed push leaves .env describing what is actually
# running rather than a tag that is not in the registry.
sed -i "s|^$tag_var=.*|$tag_var=$tag|" .env
echo
echo "deploy/.env now names $tag"

echo
echo "recreating $service"
# No --no-deps: for auth that means auth-migrate runs first, which is the point.
# Its spec changes too, because it shares the image tag, so Compose recreates it
# and the migrations run against the schema the new binary expects. goose records
# applied versions, so this is a no-op when there is nothing to apply.
$COMPOSE up -d "$service"

echo
echo "waiting for $service"
wait_http "$service" "$probe_host" 9090 "$probe_path"
report_http "$service readiness" "$probe_host" 9090 /readyz

echo
$COMPOSE ps
