docker start otomo-pg >/dev/null 2>&1; for i in $(seq 1 30); do docker exec otomo-pg pg_isready -U auth_rw -d auth >/dev/null 2>&1 && break; sleep 1; done; echo "postgres up: $(docker ps --filter name=otomo-pg --format "{{.Status}}")"
NET=otomo-test; IMG=otomo-auth:skeleton; DB='postgres://auth_rw:pw@otomo-pg:5432/auth'; KEYS=/tmp/otomo-keys
run() { docker run --rm --network $NET -e AUTH_DATABASE_URL=$DB "$@" > /tmp/o 2>&1; rc=$?; tail -1 /tmp/o | cut -c1-220; echo "exit=$rc"; }
echo "--- migrate #1 ---"; run $IMG migrate
echo "--- migrate #2 (expect no-op) ---"; run $IMG migrate
echo "--- tables ---"; docker exec otomo-pg psql -U auth_rw -d auth -Atc "select string_agg(table_name, ',' order by table_name) from information_schema.tables where table_schema='public'"
rm -rf $KEYS; mkdir -p $KEYS; chown 65532:65532 $KEYS
echo "--- genkey ---"; run -v $KEYS:/keys $IMG genkey -kid auth-test-1 -out /keys/auth.pem; ls -l $KEYS | tail -1
echo "--- genkey again, same path (expect refuse, exit 1) ---"; run -v $KEYS:/keys $IMG genkey -kid auth-test-2 -out /keys/auth.pem
echo "--- genkey dup kid, new path (expect exit 1, file removed) ---"; run -v $KEYS:/keys $IMG genkey -kid auth-test-1 -out /keys/dup.pem; echo "dup.pem exists: $(test -f $KEYS/dup.pem && echo yes || echo no)"
echo "--- serve ---"; docker rm -f otomo-auth-test >/dev/null 2>&1
docker run -d --name otomo-auth-test --network $NET -p 127.0.0.1:18080:8080 -p 127.0.0.1:19090:9090 -e AUTH_DATABASE_URL=$DB -e AUTH_SIGNING_KEY_PATH=/keys/auth.pem -v $KEYS:/keys:ro $IMG >/dev/null; sleep 2
echo "healthz: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19090/healthz)"
echo "readyz: $(curl -s -w ' %{http_code}' http://127.0.0.1:19090/readyz)"
echo "jwks: $(curl -s -w ' %{http_code}' http://127.0.0.1:18080/.well-known/jwks.json)"
echo "metrics on public: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/metrics)"
echo "pprof on public: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/debug/pprof/)"
echo "pprof on internal: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19090/debug/pprof/)"
echo "anonymous, no body (expect 400 validation_failed): $(curl -s -w ' %{http_code}' -X POST http://127.0.0.1:18080/auth/anonymous)"
echo "anonymous (expect 200 with tokens only): $(curl -s -w ' %{http_code}' -X POST -d '{"device_id":"smoke-device-0123456789abcdef"}' http://127.0.0.1:18080/auth/anonymous | cut -c1-160)"
echo "405: $(curl -s -D - -o /dev/null http://127.0.0.1:18080/auth/anonymous | grep -iE '^(HTTP|allow)' | tr -d '\r' | tr '\n' ' ')"
echo "404: $(curl -s -w ' %{http_code}' http://127.0.0.1:18080/nope)"
echo "request-id echo: $(curl -s -D - -o /dev/null -H 'X-Request-Id: abc-123' http://127.0.0.1:18080/.well-known/jwks.json | grep -i x-request-id | tr -d '\r')"
echo "--- metrics samples ---"; curl -s http://127.0.0.1:19090/metrics | grep -E '^auth_(build_info|jwks_keys_active|http_requests_total)'
echo "--- access log sample ---"; docker logs otomo-auth-test 2>&1 | grep http_request | head -2 | cut -c1-260
echo "--- SIGTERM (docker stop) ---"; docker stop -t 15 otomo-auth-test >/dev/null; echo "container exit=$(docker inspect --format '{{.State.ExitCode}}' otomo-auth-test)"; docker logs otomo-auth-test 2>&1 | tail -2 | cut -c1-200; docker rm otomo-auth-test >/dev/null
echo "--- deactivate key row; serve must exit 1 with the genkey hint ---"; docker exec otomo-pg psql -U auth_rw -d auth -Atc "update signing_key set active=false where kid='auth-test-1'"
run -e AUTH_SIGNING_KEY_PATH=/keys/auth.pem -v $KEYS:/keys:ro $IMG serve
