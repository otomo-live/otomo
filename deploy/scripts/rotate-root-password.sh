#!/bin/sh
# Rotate the break-glass root password (plan decision D6).
#
# The password lives in a 0600 file, `deploy/secrets/admin_auth/root_password`,
# that the container's `bootstrap-root` reads. Rotation is a new value in that
# file followed by `bootstrap-root -rotate`, which updates the stored argon2id
# hash and revokes every live root session in one transaction. If that command
# fails, the previous file is put back, so a failed run cannot leave a password
# that is written on disk but not stored in the database (or the reverse).
#
# It must run as root (or via sudo): the directory and the password file are owned
# by the admin-auth container's uid, 65532, and the script writes in place.
#
# Usage: sudo deploy/scripts/rotate-root-password.sh

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"
. ./scripts/lib.sh

ROOT_PASSWORD_FILE=secrets/admin_auth/root_password
PREVIOUS="$ROOT_PASSWORD_FILE.previous"

# This file chooses its own ownership from the existing one rather than assuming
# 65532, so a host that tightened it to another uid keeps that uid.
if [ ! -s "$ROOT_PASSWORD_FILE" ]; then
    die "rotate-root-password: $ROOT_PASSWORD_FILE is missing; run deploy/scripts/generate-secrets.sh"
fi
if [ ! -w "$ROOT_PASSWORD_FILE" ]; then
    die "rotate-root-password: $ROOT_PASSWORD_FILE is not writable; run this as root (the secrets directory belongs to uid 65532)"
fi
owner=$(stat -c '%u:%g' "$ROOT_PASSWORD_FILE" 2>/dev/null || true)

umask 077

# Keep the old value 0600 beside the new one until bootstrap-root has accepted it.
cp "$ROOT_PASSWORD_FILE" "$PREVIOUS"
chmod 600 "$PREVIOUS"

# 32 characters from the same URL-safe alphabet generate-secrets.sh uses.
openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c 32 > "$ROOT_PASSWORD_FILE"
chmod 600 "$ROOT_PASSWORD_FILE"
if [ -n "$owner" ]; then
    chown "$owner" "$ROOT_PASSWORD_FILE" 2>/dev/null || true
fi

echo "rotating the root password"
if $COMPOSE run --rm --no-deps admin-auth bootstrap-root -rotate; then
    rm -f "$PREVIOUS"
    echo
    echo "root password rotated. Read the new one as an admin over SSH:"
    echo "  sudo cat $DEPLOY_DIR/$ROOT_PASSWORD_FILE"
else
    # Put the old file back byte for byte, so the on-disk password and the stored
    # hash still agree. The failed command printed its own error above.
    mv -f "$PREVIOUS" "$ROOT_PASSWORD_FILE"
    chmod 600 "$ROOT_PASSWORD_FILE"
    if [ -n "$owner" ]; then
        chown "$owner" "$ROOT_PASSWORD_FILE" 2>/dev/null || true
    fi
    die "rotate-root-password: bootstrap-root -rotate failed; restored the previous password"
fi
