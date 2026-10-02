#!/bin/sh
# COM-7: bring the stack up on a fresh host, and say what is actually up when it
# is done.
#
# `docker compose up -d` is the whole of it once two prerequisites exist that YAML
# cannot express: deploy/.env with real passwords, and the signing keys (the
# player's, plus admin-auth's signing key, TOTP sealing key and break-glass root).
# This script produces the first and generates the rest, then runs that command, so
# the acceptance criterion ("docker compose up brings up infrastructure cleanly on a
# fresh VM", design/00-common-stack.md:172) is one command on a machine that has Docker
# and nothing else.
#
# It is safe to re-run. Every step checks before it acts: an existing .env is left
# alone, migrations are idempotent, a key is only generated when there is not one,
# and bootstrap-root is idempotent by design.

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

command -v docker >/dev/null 2>&1 || die "up: docker is not on PATH"
docker compose version >/dev/null 2>&1 || die "up: the docker compose plugin is not available"

# --- 1. secrets -------------------------------------------------------------

if [ ! -f .env ]; then
    echo "no deploy/.env yet; generating one"
    ./scripts/generate-secrets.sh
fi

[ -f valkey/valkey.conf ] || die "up: deploy/valkey/valkey.conf is missing; run deploy/scripts/generate-secrets.sh"
[ -d secrets ] || die "up: deploy/secrets/ is missing; run deploy/scripts/generate-secrets.sh"
[ -d secrets/admin_auth ] || die "up: deploy/secrets/admin_auth/ is missing; run deploy/scripts/generate-secrets.sh"
# The keys are written by the admin-auth container as the distroless nonroot uid,
# so the directory must belong to it. Catch the skipped `sudo chown` here, with the
# exact command, instead of letting genkey fail with a bare "permission denied".
admin_secrets_owner=$(stat -c '%u' secrets/admin_auth 2>/dev/null || echo unknown)
if [ "$admin_secrets_owner" != 65532 ]; then
    die "up: deploy/secrets/admin_auth/ must be owned by uid 65532 (the admin-auth container user); run:
  sudo chown -R 65532:65532 $PWD/secrets/admin_auth"
fi
# And traversable (0711): the key checks below test for files by name, which fails
# silently in a 0700 directory this account does not own, and would make a re-run
# try to regenerate keys that genkey refuses to overwrite.
admin_secrets_mode=$(stat -c '%a' secrets/admin_auth 2>/dev/null || echo unknown)
case "$admin_secrets_mode" in
    711|751|755) ;;
    *) die "up: deploy/secrets/admin_auth/ must be mode 0711 (found $admin_secrets_mode); run:
  sudo chmod 711 $PWD/secrets/admin_auth" ;;
esac

# The internal service keys are read by the distroless services as the
# same uid; a skipped chown would surface as the Allocator refusing to start.
[ -d secrets/service_keys ] || die "up: deploy/secrets/service_keys/ is missing; run deploy/scripts/generate-secrets.sh"
service_keys_owner=$(stat -c '%u' secrets/service_keys 2>/dev/null || echo unknown)
if [ "$service_keys_owner" != 65532 ]; then
    die "up: deploy/secrets/service_keys/ must be owned by uid 65532 (the service container user); run:
  sudo chown -R 65532:65532 $PWD/secrets/service_keys"
fi
[ -d secrets/allocator ] || die "up: deploy/secrets/allocator/ is missing; run deploy/scripts/generate-secrets.sh"
[ "$(stat -c '%u' secrets/allocator 2>/dev/null || echo unknown)" = 65532 ] \
    || die "up: deploy/secrets/allocator/ must be owned by uid 65532 (the allocator container user); run:
  sudo chown -R 65532:65532 $PWD/secrets/allocator"

# --- 2. images --------------------------------------------------------------

echo
echo "building images"
$COMPOSE build

# --- 3. infrastructure ------------------------------------------------------

echo
echo "starting postgres, valkey and the registry"
# The services come up after these, because every one of them needs a database
# that has been provisioned and a registry that answers.
$COMPOSE up -d postgres valkey registry

echo
echo "waiting for infrastructure"
wait_healthy postgres 30
wait_healthy valkey 30
wait_http "registry" registry 5000 /v2/

# --- 4. schemas, signing keys and the root account --------------------------

echo
echo "migrating auth, admin-auth, config, session and allocator"
# `--no-deps` because Postgres is already up and healthy by this point; the
# alternative is Compose re-checking a dependency this script has just waited on.
# Every migration runs before any serve: compose.yaml encodes that order in
# depends_on, but `run --rm` bypasses it, so the order is explicit here.
$COMPOSE run --rm --no-deps auth-migrate
$COMPOSE run --rm --no-deps admin-auth-migrate
$COMPOSE run --rm --no-deps config-migrate
$COMPOSE run --rm --no-deps session-migrate
$COMPOSE run --rm --no-deps allocator-migrate

if [ -s secrets/auth_signing_key.pem ]; then
    echo
    echo "signing key already present; not generating another"
else
    echo
    echo "generating the auth signing key"
    # This has to happen here, and in this order, for two reasons that are easy to
    # get wrong: genkey refuses to overwrite an existing file, so it cannot be a
    # one-shot Compose service on the dependency chain (a second `up` would fail
    # the chain and leave auth unable to start); and it inserts the public half as
    # a signing_key row, so it needs the migrated database above. `auth serve`
    # then refuses to start when the file's key has no active row
    # (services/auth/auth.go:133), which is why a key from `openssl` on the host
    # would not work.
    $COMPOSE run --rm --no-deps auth genkey
fi

# admin-auth's key material is generated here and now that the database is
# migrated, exactly as auth's signing key is above, for the same reason:
# `admin_auth genkey` writes the key file and inserts the matching signing_key
# row, so it cannot be a one-shot Compose service (a second `up` would fail the
# chain) and cannot be produced by openssl on the host. The TOTP sealing key is
# written by `genkey -totp` and needs no database; `bootstrap-root` is idempotent
# and runs on every `up` so a fresh host gets its break-glass root and an existing
# one is a no-op.
if [ -s secrets/admin_auth/totp.key ]; then
    echo
    echo "admin-auth TOTP sealing key already present; not generating another"
else
    echo
    echo "generating the admin-auth TOTP sealing key"
    $COMPOSE run --rm --no-deps admin-auth genkey -totp /run/secrets/admin_auth/totp.key
fi

if [ -s secrets/admin_auth/signing_key.pem ]; then
    echo
    echo "admin-auth signing key already present; not generating another"
else
    echo
    echo "generating the admin-auth signing key"
    $COMPOSE run --rm --no-deps admin-auth genkey
fi

# The Allocator's join-ticket key needs no database: genkey only writes the file,
# and refuses to overwrite one, so this check is what makes a re-run a no-op.
if [ -s secrets/allocator/signing_key.pem ]; then
    echo
    echo "allocator signing key already present; not generating another"
else
    echo
    echo "generating the allocator join-ticket signing key"
    $COMPOSE run --rm --no-deps allocator genkey
fi

# bootstrap-root reads the password file; generate-secrets.sh creates it and
# chowns the directory to the container's uid. A missing file is a hard error
# here rather than a crash loop inside the container.
if [ ! -s secrets/admin_auth/root_password ]; then
    echo "up: deploy/secrets/admin_auth/root_password is missing" >&2
    echo "up: run deploy/scripts/generate-secrets.sh, then re-run up.sh" >&2
    exit 1
fi

echo
echo "bootstrapping the break-glass root account"
# Idempotent and intentionally unconditional: it creates root on a fresh host and
# reports "root already exists" on every later run.
$COMPOSE run --rm --no-deps admin-auth bootstrap-root

# --- 5. everything ----------------------------------------------------------

echo
echo "starting the services"
$COMPOSE up -d

# --- 6. what is actually up -------------------------------------------------

echo
echo "waiting for services"
# /healthz and /readyz are on the metrics listener (:9090), not on the public
# :8080 - see services/auth/README.md:71 and
# services/gateway_dev/internal/server/server.go:105.
wait_http "auth liveness" auth 9090 /healthz
wait_http "auth readiness" auth 9090 /readyz
wait_http "auth JWKS" auth 8080 /.well-known/jwks.json
wait_http "admin-auth liveness" admin-auth 9090 /healthz
wait_http "admin-auth readiness" admin-auth 9090 /readyz
wait_http "admin-auth JWKS" admin-auth 8080 /.well-known/jwks.json
wait_http "config liveness" config 9090 /healthz
wait_http "config readiness" config 9090 /readyz
wait_http "patch liveness" patch 9090 /healthz
wait_http "patch readiness" patch 9090 /readyz
# Session's /readyz waits on Postgres, Valkey and both token domains' JWKS.
wait_http "session liveness" session 9090 /healthz
wait_http "session readiness" session 9090 /readyz
wait_http "dashboard liveness" dashboard 9090 /healthz
wait_http "dashboard readiness" dashboard 9090 /readyz
# The Allocator is internal-only; /readyz waits on Postgres.
wait_http "allocator readiness" allocator 9090 /readyz
# The Gameplay Proxy is ready once it has the JWKS and the server list.
wait_http "gameplay-proxy readiness" gameplay-proxy 9090 /readyz
# admin-ui is nginx, so it has no :9090 listener and its HTTP port is the only
# probe. The path is the app's own: the image answers 404 at the origin root.
wait_http "admin-ui" admin-ui 8080 /admin/
wait_http "gateway liveness" gateway 9090 /healthz
wait_http "gateway_dev liveness" gateway_dev 9090 /healthz

echo
echo "gateway readiness"
report_http "gateway" gateway 9090 /readyz
report_http "gateway_dev" gateway_dev 9090 /readyz

echo
$COMPOSE ps

# Read back rather than assumed: .env is what Compose interpolated, and a host
# that changed GATEWAY_PORT should see its own value printed here.
gateway_port=$(sed -n 's/^GATEWAY_PORT=//p' .env | tail -n 1)
gateway_dev_port=$(sed -n 's/^GATEWAY_DEV_PORT=//p' .env | tail -n 1)
gateway_bind_addr=$(sed -n 's/^GATEWAY_BIND_ADDR=//p' .env | tail -n 1)

cat <<EOF

The stack is up. Published ports, from deploy/.env:

  player edge   http://${gateway_bind_addr:-127.0.0.1}:${gateway_port:-8080}  (gateway; GATEWAY_BIND_ADDR in .env)
  staff plane   http://127.0.0.1:${gateway_dev_port:-8090}  (gateway_dev, loopback only)

Staff identity is admin-auth; its JWKS is at
http://admin-auth:8080/.well-known/jwks.json. A 503 from a gateway's /readyz means
one of its two token domains could not be fetched, not that the stack is down.

Still stubs that are not in this file: allocator and matchmaker. See the note at
the top of deploy/compose.yaml. To check player login end to end through the
player edge, run deploy/scripts/smoke-player-login.sh.
EOF
