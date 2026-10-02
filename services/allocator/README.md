# Allocator

Internal-only game-server allocation service. It owns the game-server pool and the
allocations made from it: Session asks it for a server on behalf of a party, game
servers register and heartbeat with it, and it mints the short-lived join tickets a
player presents to a game server.

The Allocator has **no Gateway route**. It is reachable only from inside the compose
network, and every route authenticates with a per-role service key (design decision
D4). Design: `design/14-launch-handoff.md`; edge and gameplay plan:
`design/15-allocator-and-edge-plan.md`.

## What exists today

- Three commands: `serve` (the default), `migrate`, and `genkey`, which writes the
  join-ticket signing key.
- Both HTTP listeners, structured JSON logging, Prometheus metrics (`allocator_`
  prefix), request IDs, panic recovery and graceful shutdown.
- A Postgres pool with a cached readiness ping; `/readyz` reports it honestly.
- Session's and the game server's service keys are loaded at start-up, so a missing or
  malformed key fails the container rather than the first request.
- The join-ticket signing key is loaded at start-up and its public half is served at
  `GET /.well-known/jwks.json`. Other API paths are a COM-5 404 until the registry and
  allocation tickets register through `server.Deps.Routes`.

## Running it

```sh
export ALLOCATOR_DATABASE_URL='postgres://allocator:pw@localhost:5432/allocator'
export ALLOCATOR_PUBLIC_ADDRESS=localhost

go run . migrate    # apply migrations/00001_init.sql
go run . genkey     # write the join-ticket signing key
go run . serve      # or just: go run .
```

`genkey` writes to `ALLOCATOR_SIGNING_KEY_PATH`, or to
`/run/secrets/allocator/signing_key.pem` when that is unset, and refuses to overwrite
an existing file.

`serve` refuses to start if the configuration is invalid, Postgres is unreachable, or
a key file cannot be read. `migrate` connects to `ALLOCATOR_DATABASE_URL` only: it does
not need the key files or `ALLOCATOR_PUBLIC_ADDRESS`.

### Tests

```sh
go test -count=1 ./...
```

The tests do not require a database.

## Configuration

Every variable is read once at start-up. `ALLOCATOR_DATABASE_URL` and
`ALLOCATOR_PUBLIC_ADDRESS` are required by `serve`; `migrate` needs only the former.
The rest have defaults, and the three key paths have defaults pointing at the compose
secrets mount. A configuration error prints **every** problem at once and exits `1`.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ALLOCATOR_LISTEN_ADDR` | no | `:8080` | Internal API listener. |
| `ALLOCATOR_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `ALLOCATOR_DATABASE_URL` | **yes** | — | `postgres://allocator:...@postgres:5432/allocator`. |
| `ALLOCATOR_DB_MAX_CONNS` | no | `8` | See the connection budget. |
| `ALLOCATOR_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `ALLOCATOR_SIGNING_KEY_PATH` | no | `/run/secrets/allocator/signing_key.pem` | PKCS#8 PEM private key that signs join tickets. Generate with `allocator genkey`. |
| `ALLOCATOR_SESSION_KEY_PATH` | no | `/run/secrets/allocator_session.key` | Base64url key Session presents when asking for a server. |
| `ALLOCATOR_GAMESERVER_KEY_PATH` | no | `/run/secrets/allocator_gameserver.key` | Base64url key game servers register and heartbeat with. |
| `ALLOCATOR_CALLBACK_KEY_PATH` | no | `/run/secrets/session_allocator.key` | Base64url key the Allocator presents when calling Session back. |
| `ALLOCATOR_SESSION_URL` | no | `http://session:8081` | Session's Compose address for callbacks. |
| `ALLOCATOR_PUBLIC_ADDRESS` | **yes** | — | Gameplay Proxy host handed to a player in a join ticket. |
| `ALLOCATOR_PUBLIC_PORT` | no | `27000` | Gameplay Proxy port handed to a player. |
| `ALLOCATOR_ISSUER` | no | `https://allocator.otomo.internal` | `iss` on every join ticket. |
| `ALLOCATOR_AUDIENCE` | no | `otomo:gameserver` | `aud` on every join ticket. |
| `ALLOCATOR_TICKET_TTL` | no | `60s` | Join-ticket lifetime; between `10s` and `10m`. |
| `ALLOCATOR_RESERVATION_TTL` | no | `60s` | How long a server is held before the party connects; between `10s` and `10m`. |
| `ALLOCATOR_HEARTBEAT_TIMEOUT` | no | `15s` | Silence after which a game server is considered gone. |
| `ALLOCATOR_REAP_INTERVAL` | no | `5s` | Reaper period; must be shorter than the heartbeat timeout. |
| `ALLOCATOR_READ_TIMEOUT` | no | `10s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `ALLOCATOR_WRITE_TIMEOUT` | no | `10s` | |
| `ALLOCATOR_IDLE_TIMEOUT` | no | `60s` | |
| `ALLOCATOR_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by both listeners. |

Keep the sum of every service's `*_DB_MAX_CONNS` below Postgres `max_connections`
(COM-14).

## Join-ticket signing key

`genkey` is a one-shot command with no database: it writes the Ed25519 key the
Allocator signs join tickets with, as a PKCS#8 PEM file with `0600` permissions, and
prints the path and the key's kid.

```sh
go run . genkey                          # $ALLOCATOR_SIGNING_KEY_PATH, else /run/secrets/allocator/signing_key.pem
go run . genkey -out /tmp/allocator.pem  # explicit path
```

It refuses to overwrite an existing file. To rotate, generate a new key file and
restart `serve` with `ALLOCATOR_SIGNING_KEY_PATH` pointing at it; tickets live for
`ALLOCATOR_TICKET_TTL` (60 s by default), so nothing accepts the old key for long after
the swap.

The kid is the RFC 7638 JWK thumbprint of the public key — the base64url SHA-256 of the
canonical `{"crv":"Ed25519","kty":"OKP","x":"..."}`. It rides in every ticket's JWT
header and in the published key set, so a verifier picks the right key by kid instead
of trying each one, and a rotation needs no out-of-band name agreement.

The public half is served at `GET /.well-known/jwks.json`. That route is unauthenticated
on purpose: game servers and the Gameplay Proxy fetch it to verify tickets, and it
carries only public key material.

## Listeners

| Port | Purpose |
|---|---|
| `8080` | Internal API. Session and game servers only; no Gateway route. |
| `9090` | `/healthz` (always plain `ok`), `/readyz` (503 until Postgres answers), `/metrics`, `/debug/pprof/`. Never routed to from outside the compose network. |

## Secrets it mounts

Read-only files under `/run/secrets`, generated by
`deploy/scripts/generate-secrets.sh`:

- `allocator/signing_key.pem` — `ALLOCATOR_SIGNING_KEY_PATH`.
- `allocator_session.key` — `ALLOCATOR_SESSION_KEY_PATH`.
- `allocator_gameserver.key` — `ALLOCATOR_GAMESERVER_KEY_PATH`.
- `session_allocator.key` — `ALLOCATOR_CALLBACK_KEY_PATH`.

Key material is never logged and never appears in an error message; a load failure
names the file and the role only.

## Game-server registry

Game servers call these with the **gameserver** service key (one shared key in M1, so
any game server can act for any `server_id`):

| Route | Answer |
|---|---|
| `POST /internal/servers/register` `{server_id, internal_addr, capacity}` | `204`; re-registering resets the server to `free` and ends its live allocation (`server_restarted`) |
| `POST /internal/servers/{server_id}/heartbeat` `{players_connected}` | `200 {"allocation": null \| {allocation_id, player_ids, expires_at}}`; the first player moves `reserved` → `active`; unknown or dead server → `404 not_registered` |
| `POST /internal/servers/{server_id}/ended` `{allocation_id}` | `204` (idempotent); a different allocation → `409 allocation_mismatch` |

A reaper runs every `ALLOCATOR_REAP_INTERVAL`. A server with no heartbeat for
`ALLOCATOR_HEARTBEAT_TIMEOUT` becomes `dead`, and its live allocation ends with
`server_dead`. It then expires any reservation whose `ALLOCATOR_RESERVATION_TTL` has
passed: the allocation becomes `expired` and the server it held, if it still points at
it, goes back to `free`. Every end sets `callback_pending`, the outbox the Session
callback drains. DB-backed tests use `ALLOCATOR_TEST_DATABASE_URL` and hold
an advisory lock for their whole run, so packages never overlap.

## Allocations

Session calls these with the **session** service key. Clients are always given the
Gameplay Proxy's public address (`ALLOCATOR_PUBLIC_ADDRESS`/`_PORT`), never a game
server's.

| Route | Answer |
|---|---|
| `POST /internal/allocations` `{party_id, player_ids}` | `201` new; `200` same party and same players (fresh tickets); `409 allocation_conflict`; `503 no_capacity` |
| `GET /internal/allocations/{id}` | the allocation, or `404` |
| `GET /internal/allocations?party_id=` | the party's latest allocation (Session's repair poll), or `404` |
| `POST /internal/allocations/{id}/tickets` `{player_id}` | a fresh ticket; `404` if the player isn't in it; `409 allocation_ended` |

Reservation is one transaction. A transaction-scoped advisory lock per party serialises
retries for one party; the free-server pick is `FOR UPDATE SKIP LOCKED`, so different
parties never take the same server. Servers need a heartbeat within the timeout and
enough capacity. Tickets are signed after commit.

## Reservation expiry

A reservation is a hold, not an assignment: if the party never connects, the server must
come back for someone else. `(*Pool).ExpireReservations` runs in the reaper every tick,
after the dead-server sweep. One statement moves every `reserved` allocation whose
`expires_at` is in the past to `expired` (`end_reason = 'expired'`, `ended_at = now()`,
`callback_pending = true`) and frees the server still pointing at it. The allocation rows
are locked `FOR UPDATE`, so a heartbeat that activates a reservation at the same moment
either wins (the reservation was used) or sees it already expired, never both. The server
update is guarded by `allocation_id = that allocation AND state = 'reserved'`, so a server
that re-registered and took a different hold is not freed by an old reservation's expiry.

## Session callback

Every path that ends an allocation — a game server reporting `ended`, the reaper's
`server_dead`, a re-registration's `server_restarted`, or reservation expiry — sets
`callback_pending`, the allocation outbox. A background drain (`internal/callback`) runs
every second and claims up to 50 due rows with `FOR UPDATE SKIP LOCKED` plus a 30 s lease,
so two Allocator instances never deliver the same event twice. It then calls Session:

```
POST {ALLOCATOR_SESSION_URL}/internal/session/allocations/{allocation_id}/ended
Authorization: Bearer <ALLOCATOR_CALLBACK_KEY_PATH>
Content-Type: application/json
{"party_id": "...", "reason": "<end_reason>"}
```

`2xx` clears `callback_pending`. Anything else — a non-2xx status or a network error,
each bounded by a 5 s per-request timeout — increments `callback_attempts` and re-arms
the row at `now() + 1, 2, 4, 8` seconds for attempts 1–4. At attempt 5 the drain gives up:
it clears `callback_pending` and logs a warning, and Session's repair poll
(`GET /internal/allocations?party_id=`) recovers the event from the allocation row. The
key is never logged.

`ALLOCATOR_CALLBACK_KEY_PATH` is read at start-up with the same base64url and ≥32-byte
validation as the service keys it accepts; a missing or malformed file fails `serve`.

## Metrics

`/metrics` on the private listener carries the HTTP instruments (`allocator_http_*`,
`allocator_build_info`) and the domain instruments below. Every counter label
combination is pre-initialised to `0`, so a series exists before the first event; the
server gauge emits all four states on every scrape.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `allocator_servers` | gauge | `state` | Game servers in the pool by state (`free`, `reserved`, `busy`, `dead`). Queried on each scrape with a 2 s timeout; a state with no servers is reported as `0`. |
| `allocator_servers_scrape_errors_total` | counter | — | Scrapes of `allocator_servers` that failed to query Postgres. The failed scrape emits no server series. |
| `allocator_allocations_total` | counter | `result` | `POST /internal/allocations` outcomes: `created`, `existing`, `no_capacity`, `conflict`, `invalid`, `error`. |
| `allocator_allocation_seconds` | histogram | — | `POST /internal/allocations` handling time in seconds; buckets `.005 .01 .025 .05 .1 .25 .5 1 2.5 5`. |
| `allocator_reaped_total` | counter | `kind` | Allocations ended by the reaper: `dead` (server missed its heartbeat) or `expired` (reservation TTL passed). |
| `allocator_callbacks_total` | counter | `result` | Session callback drain outcomes: `delivered` (2xx), `retry` (scheduled again), `gave_up` (attempts exhausted; Session's repair poll recovers the event). |
