#!/bin/sh
# COM-9: generate everything this deployment needs to hold a secret, on the host,
# once, before the first `up`.
#
# What it creates:
#
#   deploy/.env                  a copy of .env.example with every password
#                                filled in. Never overwrites an existing file;
#                                an existing one that predates a role gets that
#                                role's password appended/filled in (so far
#                                ADMIN_AUTH_RW_PASSWORD and
#                                POSTGRES_EXPORTER_PASSWORD).
#   deploy/valkey/valkey.conf    rendered from the tracked .example by
#                                substituting the Valkey password.
#   deploy/secrets/              the directory the auth signing key will be
#                                written into (by `up.sh`, which is where it
#                                gets generated - see that script for why it
#                                cannot be done here).
#   deploy/secrets/admin_auth/   the directory admin-auth's signing key, TOTP
#                                sealing key and root password live in. `up.sh`
#                                generates the two keys against the running
#                                database; this script creates the directory and
#                                the 0600 root password.
#
# What it does NOT create: the auth signing key, or admin-auth's two keys.
# services/auth/auth.go:254 and services/admin_auth/admin_auth.go:254 have
# genkey refuse to overwrite an existing file AND insert the public half as a
# signing_key row, and `serve` then refuses to start unless that row is active.
# So the keys can only be produced by the binary against a running database,
# which is a step up.sh performs after Postgres is up.
#
# Safe to re-run. Other than filling in a missing role password
# (ADMIN_AUTH_RW_PASSWORD, POSTGRES_EXPORTER_PASSWORD), an existing .env is left
# exactly as it is, so re-running cannot change a password a running database has
# already been provisioned with.

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ENV_FILE="$DEPLOY_DIR/.env"
ENV_TEMPLATE="$DEPLOY_DIR/.env.example"
VALKEY_TEMPLATE="$DEPLOY_DIR/valkey/valkey.conf.example"
VALKEY_CONF="$DEPLOY_DIR/valkey/valkey.conf"
SECRETS_DIR="$DEPLOY_DIR/secrets"
ADMIN_SECRETS_DIR="$SECRETS_DIR/admin_auth"
ROOT_PASSWORD_FILE="$ADMIN_SECRETS_DIR/root_password"

# The uid the admin-auth image runs as (gcr.io/distroless/static-debian12:nonroot).
# The directory it generates its keys in has to be owned by this uid, because the
# image has no shell with which to chown.
NONROOT_UID=65532

# Everything this script creates is a secret or a directory secrets go in, so the
# default is 0600/0700 and anything looser is set explicitly below.
umask 077

# 24 bytes of hex: 192 bits, and hex rather than base64 on purpose. These values
# are interpolated into a DSN, a libpq-style env var and a Valkey config file, and
# hex needs no escaping in any of them, in SQL, or in sed.
hex() { openssl rand -hex 24; }

read_env_value() {
    # Reads one key out of .env rather than sourcing the file: sourcing would run
    # whatever a hand-edit put there, and this script needs exactly one value.
    sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}

if [ -f "$ENV_FILE" ]; then
    # A host provisioned before a role existed has no password for it. Fill each
    # missing one in, without touching any other value, so `up.sh` and
    # provision-upgrade.sh can converge the new role. Roles added over time:
    # ADMIN_AUTH_RW_PASSWORD, then POSTGRES_EXPORTER_PASSWORD, then
    # added ALLOCATOR_RW_PASSWORD.
    changed=0
    for key in ADMIN_AUTH_RW_PASSWORD POSTGRES_EXPORTER_PASSWORD ALLOCATOR_RW_PASSWORD; do
        if [ -n "$(read_env_value "$key")" ]; then
            continue
        fi
        value=$(hex)
        if grep -q "^$key=" "$ENV_FILE"; then
            sed "s|^$key=.*|$key=$value|" "$ENV_FILE" > "$ENV_FILE.tmp"
            mv "$ENV_FILE.tmp" "$ENV_FILE"
        else
            printf '\n%s=%s\n' "$key" "$value" >> "$ENV_FILE"
        fi
        changed=1
        echo "deploy/.env already exists; added a fresh $key (0600)."
    done
    if [ "$changed" -eq 0 ]; then
        echo "deploy/.env already exists; leaving it untouched."
    else
        chmod 600 "$ENV_FILE"
    fi
else
    if [ ! -f "$ENV_TEMPLATE" ]; then
        echo "generate-secrets: $ENV_TEMPLATE is missing" >&2
        exit 1
    fi

    superuser_password=$(hex)
    auth_rw_password=$(hex)
    config_rw_password=$(hex)
    session_rw_password=$(hex)
    patch_ro_password=$(hex)
    admin_auth_rw_password=$(hex)
    allocator_rw_password=$(hex)
    postgres_exporter_password=$(hex)
    valkey_password=$(hex)

    sed \
        -e "s|^POSTGRES_SUPERUSER_PASSWORD=.*|POSTGRES_SUPERUSER_PASSWORD=$superuser_password|" \
        -e "s|^AUTH_RW_PASSWORD=.*|AUTH_RW_PASSWORD=$auth_rw_password|" \
        -e "s|^CONFIG_RW_PASSWORD=.*|CONFIG_RW_PASSWORD=$config_rw_password|" \
        -e "s|^SESSION_RW_PASSWORD=.*|SESSION_RW_PASSWORD=$session_rw_password|" \
        -e "s|^PATCH_RO_PASSWORD=.*|PATCH_RO_PASSWORD=$patch_ro_password|" \
        -e "s|^ADMIN_AUTH_RW_PASSWORD=.*|ADMIN_AUTH_RW_PASSWORD=$admin_auth_rw_password|" \
        -e "s|^ALLOCATOR_RW_PASSWORD=.*|ALLOCATOR_RW_PASSWORD=$allocator_rw_password|" \
        -e "s|^POSTGRES_EXPORTER_PASSWORD=.*|POSTGRES_EXPORTER_PASSWORD=$postgres_exporter_password|" \
        -e "s|^VALKEY_PASSWORD=.*|VALKEY_PASSWORD=$valkey_password|" \
        "$ENV_TEMPLATE" > "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    echo "wrote deploy/.env with fresh passwords (0600)."
fi

valkey_password=$(read_env_value VALKEY_PASSWORD)
if [ -z "$valkey_password" ]; then
    echo "generate-secrets: VALKEY_PASSWORD is empty in $ENV_FILE" >&2
    exit 1
fi

if [ ! -f "$VALKEY_TEMPLATE" ]; then
    echo "generate-secrets: $VALKEY_TEMPLATE is missing" >&2
    exit 1
fi

# Rendered from .env on every run, but only written when it actually changes, so a
# re-run does not touch the file's mtime. Note that a change here does not reach a
# running container until valkey is restarted:
#   docker compose -f deploy/compose.yaml restart valkey
valkey_tmp="$VALKEY_CONF.tmp"
sed "s|__VALKEY_PASSWORD__|$valkey_password|g" "$VALKEY_TEMPLATE" > "$valkey_tmp"
if [ -f "$VALKEY_CONF" ] && cmp -s "$valkey_tmp" "$VALKEY_CONF"; then
    rm -f "$valkey_tmp"
    echo "deploy/valkey/valkey.conf is already current."
else
    mv "$valkey_tmp" "$VALKEY_CONF"
    # 0644, not 0600, and this is a real concession rather than an oversight: the
    # Valkey image runs as its own non-root user, so a file owned by the deploying
    # account with 0600 is a file Valkey cannot read, and the container exits on a
    # config it cannot open. On a single-administrator host the exposure is the same
    # password that already sits in .env at 0600. On a host with other users, tighten
    # it to the image's own uid:
    #   sudo chown "$(docker run --rm valkey/valkey:8-alpine id -u)" deploy/valkey/valkey.conf
    chmod 644 "$VALKEY_CONF"
    echo "rendered deploy/valkey/valkey.conf."
fi

install -d "$SECRETS_DIR"
# 1777, and the sticky bit is the point: the auth signing key is written by the
# container (uid 65532, the distroless nonroot user), the image has no shell to
# chown with, and a 0700 directory owned by the deploying account is one that uid
# cannot create a file in. Sticky means only the owner of the key file can replace
# or delete it, and the file itself is 0600 - so this directory is writable and
# not readable by others, which is what the key generation needs and no more.
#
# The consequence, worth knowing before you need it: reading the key from this
# host requires sudo, because it is owned by 65532.
#   sudo cat deploy/secrets/auth_signing_key.pem
chmod 1777 "$SECRETS_DIR"
echo "prepared deploy/secrets/ (1777, sticky)."

# --- admin-auth secrets -----------------------------------------------------
#
# The directory is 0711, not 1777 like its parent, and owned by the distroless
# nonroot uid (65532). admin-auth's signing key and TOTP sealing key are generated
# by `up.sh` through that image, whose shell-less filesystem cannot chown anything;
# the root password file is 0600 and read by `bootstrap-root` as the same uid.
# 0711 rather than 0700: nobody else can list the directory or read a file in it
# (every file is 0600), but the deploying account can still test whether a known
# file exists, which is how up.sh decides whether to generate a key. With 0700
# those tests fail once the directory belongs to 65532, and a re-run of up.sh
# would try to regenerate keys that genkey refuses to overwrite.
if [ ! -d "$ADMIN_SECRETS_DIR" ]; then
    install -d -m 711 "$ADMIN_SECRETS_DIR"
fi
# Re-runs may find the directory already owned by uid 65532 (after the sudo chown
# below), in which case this account cannot chmod it; up.sh checks the mode.
chmod 711 "$ADMIN_SECRETS_DIR" 2>/dev/null || true

if [ -f "$ROOT_PASSWORD_FILE" ]; then
    echo "deploy/secrets/admin_auth/root_password already exists; leaving it untouched."
else
    # 32 characters, URL-safe: base64 then strip everything outside A-Za-z0-9.
    # 48 random bytes yield 64 base64 characters, so 32 survive the filter.
    openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c 32 > "$ROOT_PASSWORD_FILE"
    chmod 600 "$ROOT_PASSWORD_FILE"
    echo "wrote deploy/secrets/admin_auth/root_password (0600)."
fi

# --- internal service keys ---------------------------------------
#
# One static key per internal caller (decision D4, design/14-launch-handoff.md §4.1):
# the key a caller presents is what identifies its role, so each file is used by
# exactly one caller and checked by exactly one callee. 32 random bytes,
# base64url. Same ownership model as admin_auth/: a 0711 directory and 0600 files
# owned by the distroless nonroot uid, because the services read them as that uid.
# Never regenerated: rotating a key means deleting its file on purpose, then
# restarting both sides.
SERVICE_KEYS_DIR="$SECRETS_DIR/service_keys"
if [ ! -d "$SERVICE_KEYS_DIR" ]; then
    install -d -m 711 "$SERVICE_KEYS_DIR"
fi
chmod 711 "$SERVICE_KEYS_DIR" 2>/dev/null || true
for name in allocator_session allocator_gameserver allocator_proxy session_allocator patch_session; do
    key_file="$SERVICE_KEYS_DIR/$name.key"
    if [ -e "$key_file" ]; then
        echo "deploy/secrets/service_keys/$name.key already exists; leaving it untouched."
        continue
    fi
    if ! (umask 077 && openssl rand 32 | openssl base64 -A | tr '+/' '-_' | tr -d '=' > "$key_file"); then
        echo "generate-secrets: cannot write $key_file; if the directory already belongs to uid $NONROOT_UID, re-run with sudo" >&2
        exit 1
    fi
    chmod 600 "$key_file"
    echo "wrote deploy/secrets/service_keys/$name.key (0600)."
done

# The Allocator's join-ticket signing key is written by `allocator genkey` in its
# container (up.sh), so its directory gets the admin_auth/ treatment: 0711, owned
# by the nonroot uid.
ALLOCATOR_SECRETS_DIR="$SECRETS_DIR/allocator"
if [ ! -d "$ALLOCATOR_SECRETS_DIR" ]; then
    install -d -m 711 "$ALLOCATOR_SECRETS_DIR"
fi
chmod 711 "$ALLOCATOR_SECRETS_DIR" 2>/dev/null || true

# give the container's uid ownership it needs to write the keys, or say exactly
# what the operator has to run when this script is not root. Reading root_password
# then requires sudo, which is the D6 shape: admins read it over SSH.
if [ "$(id -u)" -eq 0 ]; then
    chown "$NONROOT_UID:$NONROOT_UID" "$ADMIN_SECRETS_DIR" "$ROOT_PASSWORD_FILE"
    echo "chowned deploy/secrets/admin_auth/ to uid $NONROOT_UID."
    chown -R "$NONROOT_UID:$NONROOT_UID" "$SERVICE_KEYS_DIR" "$ALLOCATOR_SECRETS_DIR"
    echo "chowned deploy/secrets/service_keys/ to uid $NONROOT_UID."
else
    echo "generate-secrets: allow the admin-auth container (uid $NONROOT_UID) to use deploy/secrets/admin_auth/:"
    echo "  sudo chown -R $NONROOT_UID:$NONROOT_UID $DEPLOY_DIR/secrets/admin_auth"
    echo "generate-secrets: and the internal services (uid $NONROOT_UID) to read deploy/secrets/service_keys/:"
    echo "  sudo chown -R $NONROOT_UID:$NONROOT_UID $DEPLOY_DIR/secrets/service_keys $DEPLOY_DIR/secrets/allocator"
fi

echo
echo "next: deploy/scripts/up.sh"
