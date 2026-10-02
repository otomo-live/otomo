#!/bin/sh
# End-to-end smoke test for the config image. Run it from the service directory, after
# building the image. See README.md ("End-to-end smoke test") for the prerequisites.
#
# It needs a Go toolchain as well as Docker: this service has no genkey command by
# design, so `go run ./smoke/keygen` stands in for PHP Admin Auth and writes a JWKS plus
# one signed staff token per case. nginx:alpine serves the JWKS to the container.
set -u

NET=otomo-test; PG=otomo-pg; IMG=otomo-config:skeleton; JWKHOST=otomo-jwks
DB='postgres://config_rw:pw@otomo-pg:5432/config'
TMP=${TMPDIR:-/tmp}/otomo-config-smoke
DBURL_UNREACHABLE='http://otomo-jwks.invalid/.well-known/jwks.json'

echo "== postgres =="
docker start $PG >/dev/null 2>&1
i=0; while [ $i -lt 30 ]; do docker exec $PG pg_isready -U config_rw -d config >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
echo "postgres: $(docker ps --filter name=$PG --format '{{.Status}}')"

echo "== key material (stands in for PHP Admin Auth) =="
rm -rf $TMP; mkdir -p $TMP/jwks $TMP/blobs
go run ./smoke/keygen $TMP/jwks || exit 1
chown -R 65532:65532 $TMP/blobs
docker rm -f $JWKHOST >/dev/null 2>&1
docker run -d --name $JWKHOST --network $NET -v "$TMP/jwks:/usr/share/nginx/html:ro" nginx:alpine >/dev/null
sleep 2
echo "jwks: $(docker exec $JWKHOST wget -qO- http://localhost/.well-known/jwks.json | head -c 60)…"
JWKS_URL="http://$JWKHOST/.well-known/jwks.json"

# run CMD... with the image's environment; the last line of output plus the exit code.
run() { docker run --rm --network $NET -e CONFIG_DATABASE_URL=$DB -e CONFIG_BLOB_ROOT=/blobs \
  -v "$TMP/blobs:/blobs" "$@" >$TMP/out 2>&1; rc=$?; tail -1 $TMP/out | cut -c1-200; echo "  exit=$rc"; }
tok() { cat $TMP/jwks/$1.jwt; }
psql() { docker exec $PG psql -U config_rw -d config -Atc "$1"; }

echo "== migrate #1 =="; run $IMG migrate
BEFORE=$(psql 'select count(*) from release')
echo "== migrate #2 (expect no-op; releases must not change) =="; run $IMG migrate
AFTER=$(psql 'select count(*) from release')
echo "releases: $BEFORE -> $AFTER"
psql "select string_agg(table_name, ',' order by table_name) from information_schema.tables where table_schema='public'"
psql "select channel || '=' || release_id from channel_head order by channel"

# A refusal is only meaningful if the container exits non-zero *and* names the variable,
# so both cases capture the exit code on the spot.
refuse() {
  docker run --rm --network $NET -e CONFIG_BLOB_ROOT=/blobs -v "$TMP/blobs:/blobs" "$@" >$TMP/out 2>&1
  rc=$?
  echo "  exit=$rc: $(tail -1 $TMP/out | cut -c1-200)"
}
echo "== boot refusals =="
echo "-- no CONFIG_DATABASE_URL:"
refuse $IMG serve
echo "-- no CONFIG_STAFF_JWKS_URL:"
refuse -e CONFIG_DATABASE_URL=$DB $IMG serve
echo "-- unwritable blob root:"
refuse -e CONFIG_DATABASE_URL=$DB -e CONFIG_STAFF_JWKS_URL=$JWKS_URL -e CONFIG_STAFF_ISSUER=x -e CONFIG_STAFF_AUDIENCE=y -e CONFIG_BLOB_ROOT=/proc/nope $IMG serve
echo "-- unknown command:"
refuse -e CONFIG_DATABASE_URL=$DB $IMG frobnicate

echo "== serve =="
docker rm -f otomo-config-test >/dev/null 2>&1
docker run -d --name otomo-config-test --network $NET -p 127.0.0.1:18080:8080 -p 127.0.0.1:19090:9090 \
  -e CONFIG_DATABASE_URL=$DB -e CONFIG_BLOB_ROOT=/blobs -v "$TMP/blobs:/blobs" \
  -e CONFIG_STAFF_JWKS_URL=$JWKS_URL -e CONFIG_STAFF_ISSUER=https://php-admin.otomo.internal \
  -e CONFIG_STAFF_AUDIENCE=otomo:staff $IMG >/dev/null
H=http://127.0.0.1:18080; I=http://127.0.0.1:19090
i=0; while [ $i -lt 30 ]; do curl -s -o /dev/null $I/healthz && break; i=$((i+1)); sleep 0.5; done

echo "healthz:            $(curl -s -w ' %{http_code}' $I/healthz)"
echo "readyz:             $(curl -s -w ' %{http_code}' $I/readyz)"
echo "readyz on public:   $(curl -s -o /dev/null -w '%{http_code}' $H/readyz)"
echo "metrics on public:  $(curl -s -o /dev/null -w '%{http_code}' $H/metrics)"
echo "no token:           $(curl -s -w ' %{http_code}' $H/api/admin/config/namespaces)"
echo "garbage token:      $(curl -s -w ' %{http_code}' -H 'Authorization: Bearer not.a.jwt' $H/api/admin/config/namespaces)"
echo "expired:            $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok expired)" $H/api/admin/config/namespaces)"
echo "player domain:      $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok player)" $H/api/admin/config/namespaces)"
echo "forged signature:   $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok forged)" $H/api/admin/config/namespaces)"
echo "viewer GET:         $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok viewer)" $H/api/admin/config/namespaces)"
echo "viewer POST ns:     $(curl -s -w ' %{http_code}' -X POST -H "Authorization: Bearer $(tok viewer)" $H/api/admin/config/namespaces)"
echo "live_ops -> live:   $(curl -s -w ' %{http_code}' -X POST -H "Authorization: Bearer $(tok live_ops)" $H/api/admin/config/channels/live/releases)"
echo "admin -> live:      $(curl -s -w ' %{http_code}' -X POST -H "Authorization: Bearer $(tok admin)" $H/api/admin/config/channels/live/releases)"
echo "404:                $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok viewer)" $H/api/admin/config/nope)"
echo "404 unauthenticated:$(curl -s -o /dev/null -w '%{http_code}' $H/api/admin/config/nope)"
echo "405:                $(curl -s -D - -o /dev/null -X DELETE -H "Authorization: Bearer $(tok viewer)" $H/api/admin/config/namespaces | grep -iE '^(HTTP|allow)' | tr -d '\r' | tr '\n' ' ')"
echo "405 unauthenticated:$(curl -s -o /dev/null -w '%{http_code}' -X DELETE $H/api/admin/config/namespaces)"
echo "request-id echo:    $(curl -s -D - -o /dev/null -H 'X-Request-Id: smoke-123' $H/api/admin/config/namespaces | grep -i x-request-id | tr -d '\r')"

echo "== metrics =="
curl -s $I/metrics | grep -E '^config_(build_info|token_rejected_total|http_requests_total)'
echo "== access log =="
docker logs otomo-config-test 2>&1 | grep -c http_request | sed 's/^/lines: /'
docker logs otomo-config-test 2>&1 | grep http_request | head -2 | cut -c1-240

echo "== SIGTERM (docker stop) =="
docker stop -t 15 otomo-config-test >/dev/null
echo "exit code: $(docker inspect --format '{{.State.ExitCode}}' otomo-config-test)"
docker logs otomo-config-test 2>&1 | tail -2 | cut -c1-160
docker rm otomo-config-test >/dev/null

echo "== serve with the issuer unreachable: must still listen, and must not admit =="
docker rm -f otomo-config-nojwks >/dev/null 2>&1
docker run -d --name otomo-config-nojwks --network $NET -p 127.0.0.1:18080:8080 -p 127.0.0.1:19090:9090 \
  -e CONFIG_DATABASE_URL=$DB -e CONFIG_BLOB_ROOT=/blobs -v "$TMP/blobs:/blobs" \
  -e CONFIG_STAFF_JWKS_URL=$DBURL_UNREACHABLE -e CONFIG_STAFF_ISSUER=https://php-admin.otomo.internal \
  -e CONFIG_STAFF_AUDIENCE=otomo:staff -e CONFIG_STAFF_JWKS_REFRESH=2s $IMG >/dev/null
i=0; while [ $i -lt 40 ]; do curl -s -o /dev/null $I/healthz && break; i=$((i+1)); sleep 0.5; done
sleep 3
echo "healthz:            $(curl -s -w ' %{http_code}' $I/healthz)"
echo "readyz:             $(curl -s -w ' %{http_code}' $I/readyz)"
echo "admin token:        $(curl -s -w ' %{http_code}' -H "Authorization: Bearer $(tok admin)" $H/api/admin/config/namespaces)"
echo "startup log:"; docker logs otomo-config-nojwks 2>&1 | grep -iE 'jwks|refus|error' | head -3 | cut -c1-200
docker rm -f otomo-config-nojwks >/dev/null

echo "== cleanup =="
docker rm -f $JWKHOST >/dev/null; rm -rf $TMP
echo "done"
