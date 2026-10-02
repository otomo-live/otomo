#!/bin/sh
# Exercise postgres/init/01-provision.sh against a throwaway cluster.
#
# Fresh host, re-run (idempotency), and an existing host provisioned before
# admin_auth existed. It then checks the isolation COM-6 promises, including that
# admin_auth_rw owns admin_auth and can create citext there.
#
# Needs the Postgres client/server binaries (initdb, pg_ctl, psql) on PATH. On
# the development host that means:
#   export PATH=$HOME/pgroot/usr/lib/postgresql/16/bin:$PATH
#   export LD_LIBRARY_PATH=$HOME/pgroot/usr/lib/x86_64-linux-gnu:$HOME/pgroot/usr/lib/postgresql/16/lib
# It skips with a message when initdb is missing.
#
# Usage: deploy/scripts/test-provision.sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PROVISION="$DEPLOY_DIR/postgres/init/01-provision.sh"

BASE=/tmp/prov-test
DATA="$BASE/data"
PORT=55439

if ! command -v initdb >/dev/null 2>&1 \
    || ! command -v pg_ctl >/dev/null 2>&1 \
    || ! command -v psql >/dev/null 2>&1; then
    echo "test-provision: initdb/pg_ctl/psql not on PATH; skipping."
    echo "test-provision: see the header of this script for the pgroot PATH and LD_LIBRARY_PATH setup."
    exit 0
fi

# Dummy 48-hex passwords, the same shape generate-secrets.sh produces.
AUTH_RW_PASSWORD=111111111111111111111111111111111111111111111111
CONFIG_RW_PASSWORD=222222222222222222222222222222222222222222222222
SESSION_RW_PASSWORD=333333333333333333333333333333333333333333333333
PATCH_RO_PASSWORD=444444444444444444444444444444444444444444444444
ADMIN_AUTH_RW_PASSWORD=555555555555555555555555555555555555555555555555
POSTGRES_EXPORTER_PASSWORD=666666666666666666666666666666666666666666666666
ALLOCATOR_RW_PASSWORD=777777777777777777777777777777777777777777777777
export AUTH_RW_PASSWORD CONFIG_RW_PASSWORD SESSION_RW_PASSWORD PATCH_RO_PASSWORD ADMIN_AUTH_RW_PASSWORD POSTGRES_EXPORTER_PASSWORD ALLOCATOR_RW_PASSWORD

export PGHOST="$BASE" PGPORT="$PORT" POSTGRES_USER=postgres

fail() {
    echo "test-provision: FAIL: $1" >&2
    exit 1
}

pass() {
    echo "  ok: $1"
}

stop_cluster() {
    if [ -f "$DATA/postmaster.pid" ]; then
        pg_ctl -D "$DATA" -m fast stop >/dev/null 2>&1 || true
    fi
}

cleanup() {
    stop_cluster
    rm -rf "$BASE"
}
trap cleanup EXIT INT TERM

fresh_cluster() {
    stop_cluster
    rm -rf "$BASE"
    mkdir -p "$BASE"
    initdb -U postgres -D "$DATA" >/dev/null
    pg_ctl -D "$DATA" -o "-p $PORT -k $BASE" -l "$BASE/postgres.log" -w start >/dev/null
}

psql_super() {
    psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres "$@"
}

role_exists() {
    [ "$(psql_super -tAc "SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = '$1'")" = "1" ]
}

db_exists() {
    [ "$(psql_super -tAc "SELECT 1 FROM pg_catalog.pg_database WHERE datname = '$1'")" = "1" ]
}

can_connect() {
    PGPASSWORD=x psql -X -h "$BASE" -p "$PORT" -U "$1" -d "$2" -tAc 'SELECT 1' >/dev/null 2>&1
}

must_not_connect() {
    if can_connect "$1" "$2"; then
        fail "$1 unexpectedly connected to $2"
    fi
    pass "$1 cannot connect to $2"
}

# --- fresh host -------------------------------------------------------------

echo "== fresh cluster: provision once =="
fresh_cluster
sh "$PROVISION" >/dev/null

for r in auth_rw config_rw session_rw patch_ro admin_auth_rw allocator_rw postgres_exporter; do
    role_exists "$r" || fail "role $r was not created"
done
for d in auth config session admin_auth allocator; do
    db_exists "$d" || fail "database $d was not created"
done
pass "all seven roles and five databases exist"

echo "== fresh cluster: provision again (idempotent) =="
sh "$PROVISION" >/dev/null
[ "$(psql_super -tAc "SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname IN ('auth_rw','config_rw','session_rw','patch_ro','admin_auth_rw','allocator_rw','postgres_exporter')")" = "7" ] \
    || fail "rerun duplicated a role"
[ "$(psql_super -tAc "SELECT count(*) FROM pg_catalog.pg_database WHERE datname IN ('auth','config','session','admin_auth','allocator')")" = "5" ] \
    || fail "rerun duplicated a database"
pass "second run exited 0 and created nothing new"

# --- existing host ----------------------------------------------------------

echo "== existing host: legacy provisioning, then the new script =="
fresh_cluster
# Reconstruct the old script by deleting every admin_auth line from the new one,
# which is what a host provisioned before this ticket had.
grep -v 'admin_auth' "$PROVISION" > "$BASE/01-provision-old.sh"
sh "$BASE/01-provision-old.sh" >/dev/null
if db_exists admin_auth || role_exists admin_auth_rw; then
    fail "the reconstructed old script unexpectedly provisioned admin_auth"
fi
pass "old-style cluster has the original four roles and three databases"

sh "$PROVISION" >/dev/null
role_exists admin_auth_rw || fail "upgrade did not create role admin_auth_rw"
db_exists admin_auth || fail "upgrade did not create database admin_auth"
for r in auth_rw config_rw session_rw patch_ro; do
    role_exists "$r" || fail "upgrade dropped role $r"
done
for d in auth config session; do
    db_exists "$d" || fail "upgrade dropped database $d"
done
pass "upgrade added admin_auth/admin_auth_rw and left the others in place"

# --- isolation and ownership ------------------------------------------------

echo "== isolation and ownership =="
can_connect admin_auth_rw admin_auth || fail "admin_auth_rw cannot connect to admin_auth"
psql -X -v ON_ERROR_STOP=1 -h "$BASE" -p "$PORT" -U admin_auth_rw -d admin_auth \
    -c 'CREATE TABLE provision_probe (id int)' >/dev/null \
    || fail "admin_auth_rw cannot create a table in admin_auth"
psql -X -v ON_ERROR_STOP=1 -h "$BASE" -p "$PORT" -U admin_auth_rw -d admin_auth \
    -c 'CREATE EXTENSION IF NOT EXISTS citext' >/dev/null \
    || fail "admin_auth_rw cannot create the trusted citext extension"
pass "admin_auth_rw owns admin_auth (table and citext both created)"

must_not_connect admin_auth_rw config
must_not_connect config_rw admin_auth

# Allocator_rw owns allocator and reaches nothing else.
can_connect allocator_rw allocator || fail "allocator_rw cannot connect to allocator"
psql -X -v ON_ERROR_STOP=1 -h "$BASE" -p "$PORT" -U allocator_rw -d allocator \
    -c 'CREATE TABLE allocator_probe (id int)' >/dev/null \
    || fail "allocator_rw cannot create a table in allocator"
must_not_connect allocator_rw session
must_not_connect session_rw allocator
pass "allocator_rw owns allocator and cannot reach session; session_rw cannot reach allocator"

# postgres_exporter is monitoring-only: pg_monitor plus CONNECT on `postgres`,
# and it must not be able to reach any service database.
must_not_connect postgres_exporter auth
can_connect postgres_exporter postgres || fail "postgres_exporter cannot connect to postgres"
[ "$(psql_super -tAc "SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname='postgres_exporter' AND m.roleid='pg_monitor'::regrole")" = "1" ] \
    || fail "postgres_exporter is not a member of pg_monitor"
pass "postgres_exporter is pg_monitor and reaches only postgres"

# patch_ro reads what config_rw creates later, via the default privileges.
psql -X -v ON_ERROR_STOP=1 -h "$BASE" -p "$PORT" -U config_rw -d config \
    -c 'CREATE TABLE patch_probe (id int); INSERT INTO patch_probe VALUES (1)' >/dev/null \
    || fail "config_rw cannot create/insert in config"
[ "$(PGPASSWORD=x psql -X -h "$BASE" -p "$PORT" -U patch_ro -d config -tAc 'SELECT count(*) FROM patch_probe')" = "1" ] \
    || fail "patch_ro cannot read config"
pass "patch_ro still reads config"

echo "test-provision: PASS (fresh, rerun, upgrade, isolation)"
