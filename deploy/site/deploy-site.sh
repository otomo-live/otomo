#!/bin/sh
# Redeploy the site (compose service wiki-site) from its content
# repository's branch tip. Run by hand, from cron, or by a CI job when the
# content repo's main branch moves (deploy/site/README.md). Safe to re-run: a
# commit that is already live is a no-op.
#
# The content checkout (SITE_CONTENT_DIR in deploy/.env) is deploy-only, so it
# is reset hard to origin/<branch> every time. The build runs `mkdocs --strict`
# (services/wiki): a broken page fails here, before anything is replaced, and
# the site already running keeps serving.
set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

read_env_value() { sed -n "s/^$1=//p" .env | tail -n 1; }

content=$(read_env_value SITE_CONTENT_DIR)
branch=$(read_env_value SITE_CONTENT_BRANCH)
branch=${branch:-main}
[ -n "$content" ] || die "deploy-site: SITE_CONTENT_DIR is not set in deploy/.env"
[ -d "$content/.git" ] || die "deploy-site: $content is not a git checkout"

# One deploy at a time: a burst of pushes queues here instead of racing.
exec 9> /tmp/otomo-site-deploy.lock
flock 9

git -C "$content" fetch --quiet origin "$branch"
git -C "$content" reset --quiet --hard "origin/$branch"
git -C "$content" clean -qffdx
sha=$(git -C "$content" rev-parse --short HEAD)
echo "deploy-site: content $(git -C "$content" log -1 --format='%h %s')"

running=$(docker ps -q --filter label=com.docker.compose.project=otomo --filter label=com.docker.compose.service=wiki-site)
if [ "$(read_env_value OTOMO_WIKI_SITE_TAG)" = "$sha" ] && [ -n "$running" ]; then
    echo "deploy-site: $sha is already live"
    exit 0
fi

export OTOMO_WIKI_SITE_TAG="$sha"
echo "deploy-site: building wiki-site:$sha (mkdocs --strict)"
$COMPOSE --profile site build wiki-site
$COMPOSE --profile site push wiki-site

if grep -q '^OTOMO_WIKI_SITE_TAG=' .env; then
    sed -i "s|^OTOMO_WIKI_SITE_TAG=.*|OTOMO_WIKI_SITE_TAG=$sha|" .env
else
    printf 'OTOMO_WIKI_SITE_TAG=%s\n' "$sha" >> .env
fi

$COMPOSE --profile site up -d --no-deps wiki-site
i=0
until docker run --rm --network otomo-edge alpine:latest wget -qO- -T 3 http://wiki-site:8080/healthz >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 30 ] || die "deploy-site: wiki-site did not answer /healthz within 30 s"
    sleep 1
done
echo "deploy-site: wiki-site $sha is live"
