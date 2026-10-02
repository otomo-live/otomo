#!/bin/sh
# Re-provision an existing Postgres data directory.
#
# postgres/init/01-provision.sh runs only on the first start with an empty data
# directory, so a host that already has a postgres-data volume never sees it
# again. This script runs the same file inside the running postgres container,
# which converges every service role to the password in deploy/.env and creates
# any role, database or grant that is missing (admin_auth and admin_auth_rw, for
# a host provisioned before admin-auth had its own roles; postgres_exporter, for
# one provisioned before the observability stack).
#
# It is idempotent: running it twice changes nothing. The postgres service has to
# be running; start it with deploy/scripts/up.sh first. It reads deploy/.env the
# way generate-secrets.sh does, one value at a time, rather than sourcing it.
#
# Usage:
#   deploy/scripts/provision-upgrade.sh

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

ENV_FILE="$DEPLOY_DIR/.env"

command -v docker >/dev/null 2>&1 || die "provision-upgrade: docker is not on PATH"
[ -f "$ENV_FILE" ] || die "provision-upgrade: deploy/.env is missing; run deploy/scripts/generate-secrets.sh first"

# The passwords are NOT passed on the command line (`exec -e KEY=value` would put
# them in the host's process list). The postgres container already carries every
# *_PASSWORD from deploy/.env in its environment (compose.yaml), and
# 01-provision.sh runs with `set -u`, so a container created before a key existed
# fails with that key's name. Run deploy/scripts/up.sh first: it recreates the
# container with the current .env. This check catches the common case early.
read_env_value() {
    sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}
for key in AUTH_RW_PASSWORD CONFIG_RW_PASSWORD SESSION_RW_PASSWORD PATCH_RO_PASSWORD ADMIN_AUTH_RW_PASSWORD ALLOCATOR_RW_PASSWORD POSTGRES_EXPORTER_PASSWORD; do
    [ -n "$(read_env_value "$key")" ] || die "provision-upgrade: $key is empty in deploy/.env; run deploy/scripts/generate-secrets.sh"
done

# The provisioning psql statements run inside the container, so the postgres
# service has to be up. `ps -q` prints nothing for a stopped/absent container;
# the inspect guards a container that exists but is not running.
cid=$($COMPOSE ps -q postgres 2>/dev/null || true) # shellcheck disable=SC2086
if [ -z "$cid" ] || [ "$(docker inspect --format '{{.State.Running}}' "$cid" 2>/dev/null || true)" != "true" ]; then
    die "provision-upgrade: the postgres service is not running; start it with deploy/scripts/up.sh first"
fi

echo "re-provisioning postgres (each role converges to the password in deploy/.env)"
# shellcheck disable=SC2086
$COMPOSE exec -T postgres sh /docker-entrypoint-initdb.d/01-provision.sh
