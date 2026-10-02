#!/bin/sh
# End-to-end player login through the player gateway, against the
# compose stack.
#
#   deploy/scripts/smoke-player-login.sh
#
# What it proves, all through the published player edge (never a service port):
#
#   1. POST /auth/anonymous returns a session: tokens and schema_version 1 with
#      no services hand-off (D2), a pinned iss/aud and a UUID sub. The same device_id
#      logs in to the same sub.
#   2. POST /api/player/session/me/init with that token answers 200 with the
#      profile of that same sub, and GET /me then returns it. That is the pass:
#      Session answered, which it does only after both the gateway's and
#      Session's token guards accepted the token (SE-2).
#   3. The error surface: no token -> 401 missing_token, garbage -> 401
#      invalid_token, GET on an auth route -> 405 with Allow: POST, and no 401
#      carries WWW-Authenticate.
#   4. Expiry: the access token (2 s here, see compose.smoke.yaml) runs out, /me
#      answers 401 expired, POST /auth/refresh returns a new session, and the
#      retry with the new token reaches Session again (200).
#   5. Replaying the spent refresh token is 401 invalid_token and kills its
#      successor too; logout answers 204 and its token no longer refreshes.
#
# Prerequisites: Docker with the compose plugin, curl and python3 on the host, and
# a stack that deploy/scripts/up.sh has set up at least once (deploy/.env, the
# secrets and signing keys). The script (re)builds and starts auth, session and
# the gateway with the smoke override, and on exit recreates auth without it, so
# the stack is left with its normal 15-minute tokens. Set SMOKE_KEEP=1 to leave
# the 2 s override in place for debugging.
#
# SMOKE_NO_COMPOSE=1 skips all of the Docker handling and runs only the HTTP checks
# against GATEWAY_URL, for a stack started some other way. That stack must issue
# 2 s access tokens (AUTH_ACCESS_TOKEN_TTL=2s) for the expiry checks to hold.
#
# Exit status: 0 when every check passed, 1 otherwise. One line per check.
set -u

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR" || exit 2
. "$DEPLOY_DIR/scripts/lib.sh"

PLAYER_ISSUER=https://auth.otomo.internal
PLAYER_AUDIENCE=otomo:player
SMOKE_TTL=2     # compose.smoke.yaml's AUTH_ACCESS_TOKEN_TTL, in seconds
CLOCK_SKEW=30   # GATEWAY_JWT_CLOCK_SKEW and SESSION_JWT_CLOCK_SKEW, in seconds

NO_COMPOSE=${SMOKE_NO_COMPOSE:-0}
tools="curl python3"
[ "$NO_COMPOSE" = 1 ] || tools="docker $tools"
for tool in $tools; do
    command -v "$tool" >/dev/null 2>&1 || die "smoke: $tool is not on PATH"
done
if [ "$NO_COMPOSE" != 1 ]; then
    [ -f .env ] || die "smoke: deploy/.env is missing; run deploy/scripts/generate-secrets.sh and deploy/scripts/up.sh first"
    [ -s secrets/auth_signing_key.pem ] || die "smoke: deploy/secrets/auth_signing_key.pem is missing; run deploy/scripts/up.sh once first"
fi

# The player edge as the host reaches it, from the same .env Compose reads.
bind=$(sed -n 's/^GATEWAY_BIND_ADDR=//p' .env 2>/dev/null | tail -n 1)
port=$(sed -n 's/^GATEWAY_PORT=//p' .env 2>/dev/null | tail -n 1)
case ${bind:-127.0.0.1} in 0.0.0.0 | "") bind=127.0.0.1 ;; esac
GATEWAY_URL=${GATEWAY_URL:-http://${bind:-127.0.0.1}:${port:-8080}}

SMOKE="$COMPOSE -f compose.smoke.yaml"
TMP=$(mktemp -d)
restore() {
    rm -rf "$TMP"
    if [ "$NO_COMPOSE" != 1 ] && [ "${SMOKE_KEEP:-0}" != 1 ]; then
        echo
        echo "restoring auth without the smoke override"
        $COMPOSE up -d auth >/dev/null 2>&1 || echo "smoke: could not restore auth; run: cd deploy && $COMPOSE up -d auth" >&2
    fi
}
trap restore EXIT
trap 'exit 1' INT TERM

passed=0
failed=0
pass() { passed=$((passed + 1)); echo "  PASS  $1"; }
fail() { failed=$((failed + 1)); echo "  FAIL  $1" >&2; }

# call <method> <path> [bearer] [json body]: sets STATUS; the body and headers are
# left in $TMP/body and $TMP/headers.
call() {
    _method=$1 _path=$2 _token=${3:-} _body=${4:-}
    set -- -sS -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' -X "$_method"
    [ -n "$_token" ] && set -- "$@" -H "Authorization: Bearer $_token"
    [ -n "$_body" ] && set -- "$@" -H 'Content-Type: application/json' --data "$_body"
    STATUS=$(curl "$@" "$GATEWAY_URL$_path" 2>"$TMP/curl.err") || STATUS=000
}

# json <dotted.path>: a field of the last response body; "null" for JSON null,
# "" when absent or the body is not JSON.
json() {
    python3 - "$TMP/body" "$1" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1]))
    for k in sys.argv[2].split("."):
        v = v[k]
except Exception:
    print("")
    sys.exit(0)
print("null" if v is None else v)
PY
}

# claim <jwt> <name>: a claim from the token's payload (unverified: the gateway and
# Session did the verifying; this only reads what they accepted).
claim() {
    python3 - "$1" "$2" <<'PY'
import base64, json, sys
payload = sys.argv[1].split(".")[1]
v = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4))).get(sys.argv[2])
print(",".join(v) if isinstance(v, list) else ("" if v is None else v))
PY
}

# header <name>: the last response's value of a header, matched case-insensitively.
header() {
    tr -d '\r' <"$TMP/headers" |
        awk -v want="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" '
            { i = index($0, ":") }
            i > 0 && tolower(substr($0, 1, i - 1)) == want { v = substr($0, i + 1); sub(/^ +/, "", v) }
            END { print v }'
}

# expect <label> <status> <error code or ""> : judge the last response.
expect() {
    got_code=$(json error.code)
    if [ "$STATUS" = "$2" ] && [ "$got_code" = "$3" ]; then
        pass "$1 -> $STATUS${3:+ $3}"
    else
        fail "$1 -> $STATUS ${got_code:-<no code>}, want $2${3:+ $3} ($(head -c 300 "$TMP/body" 2>/dev/null))"
    fi
    case $STATUS in
        401 | 403)
            [ -z "$(header WWW-Authenticate)" ] || fail "$1: carries WWW-Authenticate"
            ;;
    esac
}

new_device_id() {
    python3 -c 'import base64, secrets; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode())'
}

# session_ok <label>: the last response is a full login/refresh body. Sets ACCESS,
# REFRESH and SUB from it.
session_ok() {
    expect "$1" 200 ""
    ACCESS=$(json access_token)
    REFRESH=$(json refresh_token)
    SUB=$(claim "$ACCESS" sub 2>/dev/null || true)
    [ "$(json schema_version)" = 1 ] || fail "$1: schema_version is $(json schema_version), want 1"
    [ "$(json expires_in)" = "$SMOKE_TTL" ] || fail "$1: expires_in is $(json expires_in), want $SMOKE_TTL (is the smoke override applied?)"
    [ -z "$(json services)" ] || fail "$1: the body still carries a services hand-off (removed, D2)"
    [ -n "$REFRESH" ] || fail "$1: no refresh_token"
    [ "$(claim "$ACCESS" iss)" = "$PLAYER_ISSUER" ] || fail "$1: iss is '$(claim "$ACCESS" iss)'"
    [ "$(claim "$ACCESS" aud)" = "$PLAYER_AUDIENCE" ] || fail "$1: aud is '$(claim "$ACCESS" aud)'"
    [ -z "$(claim "$ACCESS" roles)" ] || fail "$1: player token carries roles"
    python3 -c 'import sys, uuid; uuid.UUID(sys.argv[1])' "$SUB" 2>/dev/null || fail "$1: sub '$SUB' is not a UUID"
}

if [ "$NO_COMPOSE" != 1 ]; then
    echo "== starting auth (2 s access tokens), session and the gateway"
    $SMOKE up -d --build auth session-migrate session gateway || die "smoke: compose up failed"

    echo
    echo "== waiting for readiness"
    # Session's and the gateway's readiness both include having fetched auth's JWKS
    # (and admin-auth's), which is what the checks below depend on.
    wait_http "auth readiness" auth 9090 /readyz 45 || die "smoke: auth never became ready"
    wait_http "session readiness" session 9090 /readyz 45 || die "smoke: session never became ready"
    wait_http "gateway readiness" gateway 9090 /readyz 45 || die "smoke: gateway never became ready"
fi

echo
echo "== player login through $GATEWAY_URL"
DEVICE=$(new_device_id)
call POST /auth/anonymous "" "{\"device_id\":\"$DEVICE\"}"
session_ok "POST /auth/anonymous"
FIRST_ACCESS=$ACCESS FIRST_REFRESH=$REFRESH FIRST_SUB=$SUB

call POST /auth/anonymous "" "{\"device_id\":\"$DEVICE\"}"
session_ok "POST /auth/anonymous (same device again)"
if [ "$SUB" = "$FIRST_SUB" ]; then pass "same device_id -> same sub"; else fail "same device_id gave sub $SUB, first was $FIRST_SUB"; fi

call POST /api/player/session/me/init "$FIRST_ACCESS"
expect "POST /api/player/session/me/init (gateway and Session guards both accept)" 200 ""
[ "$(json player_id)" = "$FIRST_SUB" ] && pass "/me/init returns the token's own profile" || fail "/me/init player_id is '$(json player_id)', want $FIRST_SUB"
PROFILE_NAME=$(json display_name) PROFILE_DISC=$(json discriminator)
call GET /api/player/session/me "$FIRST_ACCESS"
expect "GET /api/player/session/me" 200 ""
[ "$(json display_name)#$(json discriminator)" = "$PROFILE_NAME#$PROFILE_DISC" ] && pass "GET /me returns the profile /me/init made" || fail "GET /me returned $(json display_name)#$(json discriminator), want $PROFILE_NAME#$PROFILE_DISC"

echo
echo "== error surface"
call GET /api/player/session/me
expect "GET /me without a token" 401 missing_token
call GET /api/player/session/me "not-a-jwt"
expect "GET /me with garbage" 401 invalid_token
call POST /auth/anonymous "" '{"device_id":"short"}'
expect "POST /auth/anonymous with a bad device_id" 400 validation_failed
call GET /auth/anonymous
expect "GET /auth/anonymous" 405 method_not_allowed
[ "$(header Allow)" = POST ] && pass "405 carries Allow: POST" || fail "Allow header is '$(header Allow)', want POST"

echo
echo "== expiry: 401 expired -> refresh once -> retry once"
exp=$(claim "$FIRST_ACCESS" exp)
now=$(date +%s)
wait_s=$((exp + CLOCK_SKEW + 3 - now))
[ "$wait_s" -gt 0 ] || wait_s=0
echo "  waiting ${wait_s}s for the access token to pass exp + ${CLOCK_SKEW}s leeway"
sleep "$wait_s"

call GET /api/player/session/me "$FIRST_ACCESS"
expect "GET /me with the expired token" 401 expired

call POST /auth/refresh "" "{\"refresh_token\":\"$FIRST_REFRESH\"}"
session_ok "POST /auth/refresh"
if [ "$SUB" = "$FIRST_SUB" ]; then pass "refresh keeps the sub"; else fail "refresh changed sub to $SUB"; fi
ROTATED_REFRESH=$REFRESH

call GET /api/player/session/me "$ACCESS"
expect "GET /me retried with the refreshed token" 200 ""

echo
echo "== refresh reuse and logout"
call POST /auth/refresh "" "{\"refresh_token\":\"$FIRST_REFRESH\"}"
expect "POST /auth/refresh replaying the spent token" 401 invalid_token
call POST /auth/refresh "" "{\"refresh_token\":\"$ROTATED_REFRESH\"}"
expect "POST /auth/refresh with the successor after reuse (family revoked)" 401 invalid_token

call POST /auth/anonymous "" "{\"device_id\":\"$DEVICE\"}"
session_ok "POST /auth/anonymous (new family)"
call POST /auth/logout "" "{\"refresh_token\":\"$REFRESH\"}"
expect "POST /auth/logout" 204 ""
call POST /auth/refresh "" "{\"refresh_token\":\"$REFRESH\"}"
expect "POST /auth/refresh after logout" 401 invalid_token

echo
echo "smoke: $passed passed, $failed failed"
[ "$failed" -eq 0 ]
