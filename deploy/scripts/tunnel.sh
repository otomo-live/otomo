#!/bin/sh
# The SSH local forward that puts the staff control plane on the
# workstation's localhost.
#
# deploy/compose.yaml publishes gateway_dev only on the host's loopback
# interface (127.0.0.1:${GATEWAY_DEV_PORT:-8090}:8080), so the remote side of
# this forward is deliberately hard-coded as 127.0.0.1:8090 and must stay that
# way. There is no other address to forward to: the listener does not exist on
# the host's public interface, and pointing this at a hostname or a public
# address would be a different (and wrong) exposure decision, not a fix.
#
# Usage:
#   deploy/scripts/tunnel.sh [--help]
#
# Environment:
#   LOCAL_PORT   workstation port to listen on (default 8090)
#   OTOMO_SSH    SSH destination, e.g. you@play.example.com (required)
#
# The tunnel is foreground; press Ctrl+C to close it. See docs/ADMIN-ACCESS.md
# for what to do once it is up.

set -eu

usage() {
    cat <<'EOF'
Usage: tunnel.sh [--help]

Open an SSH local forward to gateway_dev on the staff host, so the staff plane
is reachable at http://localhost:<port>/admin/.

Options:
  -h, --help   Show this help and exit.

Environment:
  LOCAL_PORT   Local port to listen on (default: 8090).
  OTOMO_SSH    SSH destination, e.g. you@play.example.com (required).

The tunnel runs in the foreground. Press Ctrl+C to close it.
EOF
}

case ${1:-} in
    -h|--help)
        usage
        exit 0
        ;;
    '')
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

PORT=${LOCAL_PORT:-8090}
SSH_TARGET=${OTOMO_SSH:-}
if [ -z "$SSH_TARGET" ]; then
    echo "tunnel: set OTOMO_SSH to your server's SSH destination, e.g. OTOMO_SSH=you@play.example.com" >&2
    exit 2
fi

case $PORT in
    ''|*[!0-9]*)
        echo "tunnel: LOCAL_PORT must be a number (got '$PORT')" >&2
        exit 1
        ;;
esac

# port_in_use: 0 when something is already listening on 127.0.0.1:$PORT.
# `nc -z` is the first choice; the fallbacks exist because it is not in every
# base image and `ssh -L` would otherwise fail with a less obvious message.
port_in_use() {
    if command -v nc >/dev/null 2>&1; then
        nc -z 127.0.0.1 "$PORT" >/dev/null 2>&1
        return $?
    fi
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | grep -q "[.:]$PORT[[:space:]]"
        return $?
    fi
    if command -v netstat >/dev/null 2>&1; then
        netstat -ltn 2>/dev/null | grep -q "[.:]$PORT[[:space:]]"
        return $?
    fi
    if command -v bash >/dev/null 2>&1; then
        bash -c "exec 3<>/dev/tcp/127.0.0.1/$PORT" >/dev/null 2>&1
        return $?
    fi
    # Nothing to test with. Trying the forward is better than refusing for the
    # wrong reason; ssh reports the bind failure itself.
    return 1
}

if port_in_use; then
    echo "tunnel: localhost:$PORT is already in use." >&2
    echo "tunnel: close whatever is listening there, or pick another port:" >&2
    echo "tunnel:   LOCAL_PORT=8091 deploy/scripts/tunnel.sh" >&2
    exit 1
fi

echo "open http://localhost:$PORT/admin/ — Ctrl+C to close"

# ExitOnForwardFailure makes a bind failure exit instead of leaving a tunnel
# that silently forwards nothing. ServerAliveInterval lets a dropped link be
# noticed instead of hanging.
exec ssh -N \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=30 \
    -L "${PORT}:127.0.0.1:8090" \
    "$SSH_TARGET"
