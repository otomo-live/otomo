#!/bin/sh
# Redeploy the technical wiki (compose service wiki-docs) from the tip of its
# branch. Run by hand, from cron, or by a CI job (deploy/docs/README.md). Safe to re-run: a commit that is already live is a no-op.
#
# It builds from its own sparse checkout (DOCS_CONTENT_DIR: docs/ and
# services/wiki/ only), never from this deploy checkout, so publishing the
# docs cannot move the deploy files every other service runs from. It always
# deploys the branch tip, whatever commit triggered it, so builds that finish
# out of order cannot roll the site back. The build is `mkdocs --strict`: a
# broken page fails here, before anything is replaced, and the running site
# keeps serving.
set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

read_env_value() { sed -n "s/^$1=//p" .env | tail -n 1; }

src=$(read_env_value DOCS_CONTENT_DIR)
branch=$(read_env_value DOCS_BRANCH)
branch=${branch:-staging}
[ -n "$src" ] || die "deploy-docs: DOCS_CONTENT_DIR is not set in $DEPLOY_DIR/.env"
case $src in
    *'<'* | *'>'*) die "deploy-docs: DOCS_CONTENT_DIR still contains a placeholder ($src); set the real path, e.g. /home/$(id -un)/sites/otomo-docs" ;;
esac
# This script belongs to the deploy checkout (the one running the stack). Run from
# inside the docs checkout it would read that clone's .env and compose files and
# could recreate wiki-docs from the wrong configuration.
case $DEPLOY_DIR/ in
    "$src"/*) die "deploy-docs: this is the copy inside the docs checkout ($src); run the deploy checkout's deploy/docs/deploy-docs.sh instead" ;;
esac
[ -d "$src/.git" ] || die "deploy-docs: $src is not a git checkout"

# One deploy at a time: a burst of pushes queues here instead of racing.
exec 9> /tmp/otomo-docs-deploy.lock
flock 9

git -C "$src" fetch --quiet origin "$branch"
git -C "$src" reset --quiet --hard "origin/$branch"
git -C "$src" clean -qffdx
sha=$(git -C "$src" rev-parse --short HEAD)
echo "deploy-docs: $branch at $(git -C "$src" log -1 --format='%h %s')"
[ -d "$src/docs" ] && [ -d "$src/services/wiki" ] \
    || die "deploy-docs: $src must contain docs/ and services/wiki/ (sparse-checkout set docs services/wiki)"

running=$(docker ps -q --filter label=com.docker.compose.project=otomo --filter label=com.docker.compose.service=wiki-docs)
if [ "$(read_env_value OTOMO_WIKI_DOCS_TAG)" = "$sha" ] && [ -n "$running" ]; then
    echo "deploy-docs: $sha is already live"
    exit 0
fi

image=127.0.0.1:5000/otomo-wiki-docs:$sha
echo "deploy-docs: building $image (mkdocs --strict)"
docker build --quiet \
    --file "$src/services/wiki/dockerfile" \
    --build-context content="$src/docs" \
    --build-arg BASE_PATH=/docs/ \
    --tag "$image" "$src/services/wiki" >/dev/null
docker push --quiet "$image" >/dev/null

if grep -q '^OTOMO_WIKI_DOCS_TAG=' .env; then
    sed -i "s|^OTOMO_WIKI_DOCS_TAG=.*|OTOMO_WIKI_DOCS_TAG=$sha|" .env
else
    printf 'OTOMO_WIKI_DOCS_TAG=%s\n' "$sha" >> .env
fi

# --no-build: the image above is the one to run, even if this deploy
# checkout's services/wiki is older than the branch.
$COMPOSE up -d --no-deps --no-build wiki-docs
i=0
until docker run --rm --network otomo-edge alpine:latest wget -qO- -T 3 http://wiki-docs:8080/healthz >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 30 ] || die "deploy-docs: wiki-docs did not answer /healthz within 30 s"
    sleep 1
done
echo "deploy-docs: wiki-docs $sha is live"
