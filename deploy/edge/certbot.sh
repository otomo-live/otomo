#!/bin/sh
# The host side of the edge's certificate, via the distribution's
# certbot. The edge container never talks to Let's Encrypt; this script and
# certbot's own systemd timer are the only things that do.
#
#   sudo deploy/edge/certbot.sh install                 # certbot, webroot, deploy hook
#   sudo deploy/edge/certbot.sh dry-run                 # staging CA; costs no quota
#   sudo deploy/edge/certbot.sh issue --confirm-single-issuance
#   sudo deploy/edge/certbot.sh status                  # cert, timer, renewal dry-run
#
# READ BEFORE ISSUING. Let's Encrypt allows 50 new certificates per week for the
# whole registered domain, and if your host name sits under a domain you share (a
# school's, a company's, a hosting provider's), everyone on it draws from the same 50.
# So a real certificate is requested exactly once, after a successful dry-run,
# and never again: certbot.timer renews it (renewals of an existing certificate
# are exempt from the new-certificate limit), and the deploy hook reloads the
# edge. `issue` enforces this:
#   - it refuses when a certificate for the name already exists on this host;
#   - it refuses without a successful `dry-run` in the last 2 hours;
#   - it refuses when crt.sh shows a certificate for the name issued in the
#     last 7 days (someone already spent one);
#   - it needs --confirm-single-issuance.
set -eu

NAME=${OTOMO_PUBLIC_HOST:-}
STATE_DIR=/var/lib/otomo-edge
WEBROOT=$STATE_DIR/acme
DRY_RUN_MARKER=$STATE_DIR/dry-run-ok
HOOK=/usr/local/sbin/otomo-edge-reload
HERE=$(cd "$(dirname "$0")" && pwd)

die() { echo "certbot.sh: $*" >&2; exit 1; }

need_name() {
    [ -n "$NAME" ] || die "set OTOMO_PUBLIC_HOST to your public host name, e.g. sudo OTOMO_PUBLIC_HOST=play.example.com $0 $1"
}
[ "$(id -u)" -eq 0 ] || die "run with sudo"

common_args() {
    # --register-unsafely-without-email: Let's Encrypt no longer sends expiry
    # mail, and renewal is automatic. ECDSA P-256 keeps handshakes small.
    echo "certonly --webroot -w $WEBROOT -d $NAME --non-interactive --agree-tos \
--register-unsafely-without-email --key-type ecdsa --elliptic-curve secp256r1 \
--deploy-hook $HOOK"
}

cmd_install() {
    command -v certbot >/dev/null 2>&1 || { apt-get update -q && apt-get install -y -q certbot; }
    install -d -m 755 "$WEBROOT"
    install -m 755 "$HERE/certbot-deploy-hook" "$HOOK"
    systemctl enable --now certbot.timer
    echo "installed: certbot $(certbot --version 2>&1 | awk '{print $2}'), webroot $WEBROOT, hook $HOOK, certbot.timer enabled"
}

# The challenge path has to work from the internet before any real request.
check_webroot() {
    probe="otomo-probe-$(date +%s)"
    install -d -m 755 "$WEBROOT/.well-known/acme-challenge"
    echo "$probe" > "$WEBROOT/.well-known/acme-challenge/$probe"
    got=$(curl -fsS --max-time 10 "http://$NAME/.well-known/acme-challenge/$probe" || true)
    rm -f "$WEBROOT/.well-known/acme-challenge/$probe"
    [ "$got" = "$probe" ] || die "http://$NAME/.well-known/acme-challenge/ does not serve the webroot (is the edge up, and is port 80 open?)"
    echo "webroot reachable over http://$NAME/"
}

cmd_dry_run() {
    need_name dry-run
    command -v certbot >/dev/null 2>&1 || die "certbot is not installed; run: $0 install"
    check_webroot
    rm -f "$DRY_RUN_MARKER"
    # shellcheck disable=SC2046
    certbot $(common_args) --dry-run
    date +%s > "$DRY_RUN_MARKER"
    echo "dry-run succeeded against the staging CA (no quota used)"
}

recent_issuance() {
    # crt.sh lists every publicly logged certificate. Informational and best
    # effort: if it cannot be reached, say so and let the operator decide.
    since=$(date -u -d '7 days ago' +%Y-%m-%d)
    json=$(curl -fsS --max-time 30 "https://crt.sh/?q=$NAME&output=json" 2>/dev/null) || {
        echo "warning: crt.sh unreachable; cannot check for recent certificates" >&2
        return 1
    }
    echo "$json" | tr '{' '\n' | grep -o '"not_before":"[0-9-]*' | cut -d'"' -f4 \
        | awk -v s="$since" '$0 >= s' | grep -q . && return 0
    return 1
}

cmd_issue() {
    need_name issue
    [ "${1:-}" = "--confirm-single-issuance" ] || die "issue needs --confirm-single-issuance (see the header of this script)"
    command -v certbot >/dev/null 2>&1 || die "certbot is not installed; run: $0 install"
    [ ! -e "/etc/letsencrypt/live/$NAME" ] \
        || die "a certificate for $NAME already exists; certbot.timer renews it. Never issue again."
    [ -f "$DRY_RUN_MARKER" ] || die "no successful dry-run recorded; run: $0 dry-run"
    age=$(( $(date +%s) - $(cat "$DRY_RUN_MARKER") ))
    [ "$age" -le 7200 ] || die "the last successful dry-run is $((age / 60)) minutes old; run: $0 dry-run"
    if recent_issuance; then
        die "crt.sh shows a certificate for $NAME issued in the last 7 days; investigate before spending another"
    fi
    check_webroot
    # shellcheck disable=SC2046
    certbot $(common_args)
    rm -f "$DRY_RUN_MARKER"
    echo "issued. From now on certbot.timer renews it; do not run issue again."
}

cmd_status() {
    need_name status
    certbot certificates 2>/dev/null | sed -n '/Certificate Name/,/Private Key Path/p'
    systemctl list-timers certbot.timer --no-pager | head -3
    if [ -e "/etc/letsencrypt/live/$NAME" ]; then
        # Staging CA again: exercises renewal and the saved config, costs nothing.
        certbot renew --dry-run --cert-name "$NAME" 2>&1 | tail -3
    fi
}

case "${1:-}" in
    install) cmd_install ;;
    dry-run) cmd_dry_run ;;
    issue) shift; cmd_issue "$@" ;;
    status) cmd_status ;;
    *) sed -n '2,20p' "$0"; exit 2 ;;
esac
