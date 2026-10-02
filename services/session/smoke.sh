#!/bin/sh
# End-to-end smoke test for the Session service, against binaries built from the working
# tree rather than an image, so it runs on a machine with no Docker.
#
# It checks the two things a unit test cannot: that a real process, with real Postgres,
# real Valkey and two real JWKS endpoints, comes up and answers, and that the SES-A1
# boundary holds at the HTTP layer in a deployment — a player token on a staff route is
# refused, and the refusal names a reason an operator can act on.
#
# Every route's handler is still a 501 stub (see internal/api/stubs.go), so a 501 here
# means "admitted by the guard, nothing behind it yet" — which is exactly what makes these
# assertions meaningful before any handler exists.
#
# Prerequisites:
#   - Postgres reachable at SESSION_DATABASE_URL, with this service's migrations applicable
#   - Valkey reachable at SESSION_VALKEY_URL
#   - go, curl
#
# Usage:
#   SESSION_DATABASE_URL='postgres://user:pw@127.0.0.1:5432/session_smoke' \
#   SESSION_VALKEY_URL='valkey://default:pw@127.0.0.1:6379/0' \
#     sh smoke.sh
set -u

if [ -z "${SESSION_DATABASE_URL:-}" ] || [ -z "${SESSION_VALKEY_URL:-}" ]; then
	echo "both SESSION_DATABASE_URL and SESSION_VALKEY_URL must be set; see the header of this file" >&2
	exit 2
fi

PUB=127.0.0.1:18080          # the public listener
INT=127.0.0.1:19090          # the metrics listener
CB=127.0.0.1:18081           # the internal listener (the Allocator's callback)
JWKS=127.0.0.1:18099         # keygen's stand-in JWKS endpoint
TMP=$(mktemp -d)
BIN="$TMP/bin"
mkdir -p "$BIN"

pass=0
fail=0

cleanup() {
	[ -n "${SERVE_PID:-}" ] && kill "$SERVE_PID" 2>/dev/null
	[ -n "${JWKS_PID:-}" ] && kill "$JWKS_PID" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

ok()   { echo "ok   - $1"; pass=$((pass + 1)); }
bad()  { echo "FAIL - $1"; fail=$((fail + 1)); }

check() { # check <label> <want> <got>
	if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1: want $2, got $3"; fi
}

has() { # has <label> <needle> <file>
	if grep -q "$2" "$3" 2>/dev/null; then ok "$1"; else
		bad "$1: $2 not in $(head -c 200 "$3" 2>/dev/null)"
	fi
}

# expect <method> <path> <token|-> <status> <needle|-> <label>
# Sends the request, checks the status, and greps the body for a COM-5 code.
expect() {
	_body="$TMP/body"
	if [ "$3" = "-" ]; then
		_status=$(curl -s -o "$_body" -w '%{http_code}' -X "$1" "http://$PUB$2")
	else
		_status=$(curl -s -o "$_body" -w '%{http_code}' -X "$1" \
			-H "Authorization: Bearer $3" "http://$PUB$2")
	fi
	check "$6" "$4" "$_status"
	if [ "$5" != "-" ]; then has "$6: body names $5" "$5" "$_body"; fi
}

echo "=== build ==="
( cd "$(dirname "$0")" && go build -o "$BIN/session" . && go build -o "$BIN/keygen" ./smoke/keygen ) || {
	echo "the build failed" >&2
	exit 1
}
echo "built $BIN/session and $BIN/keygen"

echo "=== keygen: two key pairs, two JWKS documents ==="
"$BIN/keygen" init "$TMP" || exit 1
test -s "$TMP/player.jwks.json" && ok "player.jwks.json written" || bad "player.jwks.json missing"
test -s "$TMP/staff.jwks.json" && ok "staff.jwks.json written" || bad "staff.jwks.json missing"

"$BIN/keygen" serve "$TMP" "$JWKS" >"$TMP/jwks.log" 2>&1 &
JWKS_PID=$!
for _ in $(seq 1 50); do
	[ "$(curl -s -o /dev/null -w '%{http_code}' "http://$JWKS/player.jwks.json")" = 200 ] && break
	sleep 0.1
done
check "the player jwks endpoint answers" 200 "$(curl -s -o /dev/null -w '%{http_code}' "http://$JWKS/player.jwks.json")"

echo "=== migrate ==="
SESSION_DATABASE_URL="$SESSION_DATABASE_URL" "$BIN/session" migrate >"$TMP/migrate1.log" 2>&1
check "migrate exits 0" 0 "$?"
SESSION_DATABASE_URL="$SESSION_DATABASE_URL" "$BIN/session" migrate >"$TMP/migrate2.log" 2>&1
check "migrate is idempotent" 0 "$?"

echo "=== serve ==="
SESSION_DATABASE_URL="$SESSION_DATABASE_URL" \
SESSION_VALKEY_URL="$SESSION_VALKEY_URL" \
SESSION_LISTEN_ADDR="$PUB" \
SESSION_METRICS_ADDR="$INT" \
SESSION_INTERNAL_ADDR="$CB" \
SESSION_PLAYER_JWKS_URL="http://$JWKS/player.jwks.json" \
SESSION_PLAYER_ISSUER=https://auth.otomo.internal \
SESSION_PLAYER_AUDIENCE=otomo:player \
SESSION_STAFF_JWKS_URL="http://$JWKS/staff.jwks.json" \
SESSION_STAFF_ISSUER=https://php-admin.otomo.internal \
SESSION_STAFF_AUDIENCE=otomo:staff \
	"$BIN/session" serve >"$TMP/serve.log" 2>&1 &
SERVE_PID=$!

check "healthz answers as soon as the listener is up" 200 "$(curl -s -o /dev/null -w '%{http_code}' "http://$INT/healthz" --retry 20 --retry-connrefused --retry-delay 0 -m 10)"
_ready=""
for _ in $(seq 1 100); do
	_ready=$(curl -s -o "$TMP/readyz" -w '%{http_code}' "http://$INT/readyz")
	[ "$_ready" = 200 ] && break
	sleep 0.2
done
check "readyz reaches 200 once both jwks are fetched" 200 "$_ready"
if [ "$_ready" != 200 ]; then
	echo "--- readyz said: $(cat "$TMP/readyz")"
	echo "--- serve log ---"; tail -20 "$TMP/serve.log"
fi

echo "=== tokens ==="
PTOK=$("$BIN/keygen" token "$TMP" player 018f4a3e-1c2d-7abc-8def-0123456789ab)
STOK=$("$BIN/keygen" token "$TMP" staff acct-1 viewer)
LTOK=$("$BIN/keygen" token "$TMP" staff acct-2 live_ops)
NTOK=$("$BIN/keygen" token "$TMP" staff acct-3)   # no roles at all
for _t in "$PTOK" "$STOK" "$LTOK" "$NTOK"; do
	case "$_t" in *.*.*) ;; *) bad "a token was not minted: $_t" ;; esac
done
ok "four tokens minted"

# A token whose signature is one byte different: the structure and the claims are intact,
# so this is the case that must fail on the signature rather than on the parse.
_sig=$(printf '%s' "$PTOK" | cut -d. -f3)
_first=$(printf '%s' "$_sig" | cut -c1)
if [ "$_first" = A ]; then _swap=B; else _swap=A; fi
TTOK="$(printf '%s' "$PTOK" | cut -d. -f1,2).$_swap$(printf '%s' "$_sig" | cut -c2-)"

echo "=== the player domain ==="
expect GET /api/player/session/me -         401 missing_token      "a player route refuses a request with no token"
expect GET /api/player/session/me "$PTOK"   501 not_implemented    "a player token reaches the player handler"
expect GET /api/player/session/me "$STOK"   401 invalid_signature  "a staff token is refused on a player route"
expect GET /api/player/session/me "$TTOK"   401 invalid_signature  "a token with a bad signature is refused"
expect GET /api/player/session/me "$PTOK."  401 invalid_token      "a malformed token is refused as malformed"

echo "=== the staff domain ==="
expect GET /api/admin/session/players "$STOK" 501 not_implemented   "a viewer reaches the staff read handler"
expect GET /api/admin/session/players "$NTOK" 403 insufficient_role "a staff token with no roles is forbidden"
expect GET /api/admin/session/players "$PTOK" 401 invalid_signature "a player token is refused on a staff route"
expect GET /api/admin/session/players -       401 missing_token     "a staff route refuses a request with no token"

echo "=== the role bar on force-disband ==="
expect POST /api/admin/session/parties/018f4a3e-1c2d-7abc-8def-0123456789ab/disband "$STOK" 403 insufficient_role "a viewer cannot force-disband"
expect POST /api/admin/session/parties/018f4a3e-1c2d-7abc-8def-0123456789ab/disband "$LTOK" 501 not_implemented   "live_ops passes the role gate"

echo "=== method and path handling ==="
expect DELETE /api/player/session/me "$PTOK" 405 method_not_allowed "wrong method on a known path is 405"
_allow=$(curl -s -D - -o /dev/null -X DELETE -H "Authorization: Bearer $PTOK" \
	"http://$PUB/api/player/session/me" | grep -i '^allow:' | tr -d '\r' | sed 's/^[Aa]llow: //')
check "the 405 names the allowed methods" "GET, HEAD, PATCH" "$_allow"
expect GET /nope "$PTOK" 404 not_found "an unknown path is 404"
expect GET /nope -       401 missing_token "the fallback is guarded too"

echo "=== the internal listener is not on the public one ==="
# Without a token these answer 401, not 404: the public listener's fallback is guarded like
# every other route, so an unauthenticated caller cannot even learn that the path exists.
# The 404 is what an authenticated caller gets for a path that is not a session route.
check "/metrics on the internal listener" 200 "$(curl -s -o /dev/null -w '%{http_code}' "http://$INT/metrics")"
expect GET /metrics -       401 missing_token "an unauthenticated public request learns nothing about /metrics"
expect GET /metrics "$PTOK" 404 not_found     "/metrics is not a public route"
expect GET /healthz "$PTOK" 404 not_found     "/healthz is not a public route"
expect GET /debug/pprof/ "$PTOK" 404 not_found "/debug/pprof/ is not a public route"

echo "=== the Allocator's callback is only on the internal listener ==="
# No SESSION_CALLBACK_KEY_PATH here, so the internal listener admits nobody.
CALLBACK=/internal/session/allocations/018f4a3e-1c2d-7abc-8def-0123456789a1/ended
check "the callback without a key is 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' -X POST -d '{}' "http://$CB$CALLBACK")"
expect POST "$CALLBACK" "$PTOK" 404 not_found "the callback is not a public route"
check "the callback is not on the metrics listener" 404 "$(curl -s -o /dev/null -w '%{http_code}' -X POST -d '{}' "http://$INT$CALLBACK")"

echo "=== request id and metrics ==="
_rid=$(curl -s -D - -o /dev/null -H 'X-Request-Id: smoke-1' -H "Authorization: Bearer $PTOK" \
	"http://$PUB/api/player/session/me" | grep -i '^x-request-id:' | tr -d '\r' | sed 's/^[Xx]-[Rr]equest-[Ii]d: //')
check "an inbound request id is echoed" "smoke-1" "$_rid"
_rid=$(curl -s -D - -o /dev/null -H "Authorization: Bearer $PTOK" \
	"http://$PUB/api/player/session/me" | grep -i '^x-request-id:' | tr -d '\r' | sed 's/^[Xx]-[Rr]equest-[Ii]d: //')
[ -n "$_rid" ] && ok "a request id is minted when none arrives ($_rid)" || bad "no request id was minted"

curl -s "http://$INT/metrics" >"$TMP/metrics"
has "metrics count the request by route and status" \
	'session_http_requests_total{method="GET",route="/api/player/session/me",status="501"}' "$TMP/metrics"
has "metrics count the refusals by domain and reason" \
	'session_token_rejected_total{domain="player",reason="missing_token"}' "$TMP/metrics"
has "metrics count the cross-domain refusal" \
	'session_token_rejected_total{domain="player",reason="invalid_signature"}' "$TMP/metrics"
has "metrics expose the build" 'session_build_info' "$TMP/metrics"
if grep -q "018f4a3e-1c2d-7abc-8def-0123456789ab" "$TMP/metrics"; then
	bad "the player id leaked into a metric label"
else
	ok "no player id in the metrics"
fi

echo "=== graceful shutdown ==="
# wait reaps the child and gives its real exit status, which is what has to be 0. The
# watchdog is what makes it a test rather than a hang: kill -0 cannot tell a running
# process from a zombie, so polling it would report "ignored SIGTERM" for a process that
# had already exited.
kill -TERM "$SERVE_PID"
( sleep 10; kill -9 "$SERVE_PID" 2>/dev/null ) &
_watchdog=$!
wait "$SERVE_PID"
_rc=$?
kill "$_watchdog" 2>/dev/null
wait "$_watchdog" 2>/dev/null
check "SIGTERM exits 0" 0 "$_rc"
has "the shutdown is logged" 'shutdown complete' "$TMP/serve.log"
SERVE_PID=

echo
echo "=== $pass passed, $fail failed ==="
[ "$fail" = 0 ]
