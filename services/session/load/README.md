# Session smoke and load tests

Both run against a deployed stack (`deploy/scripts/up.sh`), through the player gateway.

## CI levels

```sh
for level in sanity functional integration security scaling; do
  sh ci/services/otomo-session/unit_test.sh "$level" || break
done
```

## Smoke (Hurl 4 or later)

```sh
hurl --test --variable gateway=http://127.0.0.1:8080 services/session/smoke/session.hurl
```

It makes two new players and walks them through profile, presence (including the 429),
friends, block, party, ready, launch (ends `forming` with no game servers, or `in_game`),
leave and the 401 boundaries.

## Load (k6)

The player gateway limits each client IP to 20 requests a second (5 for logins), so from
one machine raise the limits for the test window and put them back afterwards:

```sh
# deploy/.env for the test window only
GATEWAY_RATE_LIMIT_RPS=2000
GATEWAY_RATE_LIMIT_BURST=4000
GATEWAY_LOGIN_RATE_LIMIT_RPS=100
GATEWAY_LOGIN_RATE_LIMIT_BURST=200
```

Then, with Grafana or Prometheus open on `go_goroutines{job="session"}`:

```sh
k6 run -e GATEWAY=http://127.0.0.1:8080 -e RUN=$(date +%s) services/session/load/presence-longpoll.js
```

`CLIENTS` (default 2000), `RAMP` (2m) and `DURATION` (10m) can be changed with `-e`.
Each client logs in once, then heartbeats every 20 s and long-polls back to back. The
script fails when p95 heartbeat is 50 ms or more, or when any request fails.

Targets (SE-9): 2,000 clients, heartbeat p95 under 50 ms, under 0.1 % failed long-polls,
`otomo_online_players` near 2,000 at steady state, and Session's `go_goroutines` back to
its starting value two minutes after the run.
