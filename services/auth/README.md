# Auth

Player identity service. Owns player accounts, the Ed25519 signing keys, access-token
issuance and refresh tokens. The login response carries tokens only (no `services` hand-off, D2).

**What exists today:** config, HTTP server, structured logging, metrics, graceful
shutdown, the AUTH-1 schema, signing-key management (AUTH-2), token issuance (AUTH-3),
device login (AUTH-4, `POST /auth/anonymous`), refresh-token rotation with family
revocation (AUTH-5, `POST /auth/refresh`, `POST /auth/logout`). The AUTH-8
`services` hand-off payload was removed on 2026-09-29 (D2). See "Scope" at the
bottom.

Source of truth for this service: `design/06-auth-identity-contract.md` (claim shape, JWKS
format), `design/07-auth-techspec.md` (data model, key management), `design/00-common-stack.md`
(service conventions). `design/08-auth-skeleton-plan.md` is the plan this skeleton was built
from and records the decisions listed below.

---

## Running it

Key generation and migrations are separate one-shot steps. The service never generates a
key at startup: doing so would invalidate every outstanding token on every restart.

```sh
export AUTH_DATABASE_URL='postgres://auth_rw:pw@localhost:5432/auth'
export AUTH_SIGNING_KEY_PATH=./auth_signing_key.pem

go run . migrate                             # create the AUTH-1 tables and the AUTH-5 indexes
go run . genkey                              # writes the key, inserts the signing_key row
go run . serve                               # or just: go run .
```

`genkey` refuses to overwrite an existing `-out` file. If the database insert fails it
deletes the file it just wrote, so you can rerun with the same `-kid` — but it never
touches a pre-existing file.

The same commands work in the container, because the entrypoint is the binary:

```sh
mkdir -p keys && chown 65532 keys        # the image runs as uid 65532 and must be able to write here
docker run --rm --env-file auth.env -v "$PWD/keys:/keys" otomo-auth:staging genkey -out /keys/auth.pem
docker run --rm --env-file auth.env otomo-auth:staging migrate
docker run --rm --env-file auth.env -p 8080:8080 otomo-auth:staging serve
```

### Commands

| Command | Does |
|---|---|
| `serve` (default) | Runs both listeners until `SIGTERM`/`SIGINT`, then shuts down gracefully. |
| `migrate` | Applies the embedded goose migrations. Idempotent. |
| `genkey` | Generates an Ed25519 key, writes it as PKCS#8 PEM (`0600`), inserts the `signing_key` row. Flags: `-kid`, `-out`. |

Unknown commands print usage and exit `2`. A configuration error prints **every** problem
at once and exits `1`.

---

## Configuration

Every variable is read once at startup. `AUTH_DATABASE_URL` is required by all three
commands; `AUTH_SIGNING_KEY_PATH` is required by `serve`
only (for `genkey` the key path is just the default for `-out`). The process exits `1` at boot rather than starting in a half-working
state — including when Postgres is unreachable. Compose's restart policy is the retry loop;
a service that cannot reach its database has nothing useful to serve.

See `.env.example` for a copy-pasteable template.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `AUTH_LISTEN_ADDR` | no | `:8080` | Public listener. Gateway proxies `/auth/*` here. |
| `AUTH_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `AUTH_DATABASE_URL` | **yes** | — | `postgres://auth_rw:...@postgres:5432/auth` |
| `AUTH_DB_MAX_CONNS` | no | `8` | See the connection budget below. |
| `AUTH_SIGNING_KEY_PATH` | **yes** (`serve`) | — | PKCS#8 PEM, mounted read-only. |
| `AUTH_ISSUER` | no | `https://auth.otomo.internal` | Must equal Gateway's `GATEWAY_PLAYER_ISSUER` exactly. |
| `AUTH_AUDIENCE` | no | `otomo:player` | Must equal Gateway's `GATEWAY_PLAYER_AUDIENCE`. |
| `AUTH_ACCESS_TOKEN_TTL` | no | `15m` | Contract §6. |
| `AUTH_REFRESH_TOKEN_TTL` | no | `720h` | 30 days, sliding: every refresh issues a successor with a fresh TTL. No absolute cap in M1. |
| `AUTH_READ_TIMEOUT` | no | `10s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `AUTH_WRITE_TIMEOUT` | no | `30s` | |
| `AUTH_IDLE_TIMEOUT` | no | `120s` | |
| `AUTH_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by both listeners. |
| `AUTH_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `AUTH_TEST_DATABASE_URL` | no | — | Read only by the `internal/store` tests. Never set in a deployment. |

### Connection budget (COM-14)

`AUTH_DB_MAX_CONNS` defaults to `8`. Keep the sum of every service's `MaxConns` below
Postgres's `max_connections`, minus a margin for admin sessions and the one-shot `migrate`
container. Raise it only alongside the Postgres setting.

---

## HTTP surface

### Public listener (`AUTH_LISTEN_ADDR`)

| Route | Status |
|---|---|
| `GET /.well-known/jwks.json` | Live. Pre-serialized bytes behind an `atomic.Pointer`, `Cache-Control: no-store`. |
| `POST /auth/anonymous` | Live. `{"device_id"}` → `200 {"schema_version":1,"access_token","expires_in","refresh_token"}` (design/07 §7; tokens only), each login a new refresh family; a bad body or `device_id` is `400 validation_failed`. `device_id` must match `^[A-Za-z0-9_-]{22,128}$` and is stored only as its hex SHA-256 (`identity_binding.external_id`, method `device`). The same `device_id` always yields the same `sub`. |
| `POST /auth/refresh` | Live. `{"refresh_token"}` → `200` in the login shape with a new `refresh_token` (a fresh 720 h expiry each time). Any unusable token — missing, malformed, unknown, expired, revoked or reused — is `401 invalid_token`; malformed JSON is `400 validation_failed`. Presenting an already-exchanged token revokes its whole family. |
| `POST /auth/logout` | Live. `{"refresh_token"}` → revokes that token's family; always `204`, whatever the body, except a database failure (`500`). |
| anything else | `404` or `405`, in the error shape below. |

Paths under `/auth/` are illustrative (contract §8). Gateway proxies `/auth/` as a wildcard
prefix, so they can change without a Gateway change. `/.well-known/jwks.json` is **not**
proxied — Gateway fetches it directly (contract §1).

Middleware, outermost first: request ID → access log → metrics → `recover` → mux. `recover`
sits innermost so a panicking handler still produces its access-log line, its metrics sample
and a `500` body carrying the request ID, instead of unwinding past them.

### Internal listener (`AUTH_METRICS_ADDR`)

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200 ok` once the process is listening. |
| `GET /readyz` | `200` once startup finished and the cached Postgres ping is healthy; `503` otherwise. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

The Postgres ping behind `/readyz` is cached for 10s, so probes do not become database load.
None of these routes exist on the public listener — they return the `404` body below.

### Error shape (COM-5)

Every 4xx and 5xx from the public listener, including `404` and `405`:

```json
{ "error": { "code": "not_found", "message": "...", "request_id": "..." } }
```

`request_id` echoes the inbound `X-Request-Id`, or a fresh UUIDv4 if Gateway did not supply
one; the same value is returned in the `X-Request-Id` response header. `api.WriteError` is
the only thing that writes an error status.

### Metrics and logs (COM-10)

Metric names are `auth_<thing>_<unit>`, labels are limited to `route`, `method`, `status`,
`result` and `new_account`, each with a fixed set of values:

- `auth_http_requests_total{route,method,status}`
- `auth_http_request_duration_seconds{route,method}`
- `auth_jwks_keys_active`
- `auth_build_info{version}`
- `auth_logins_total{result,new_account}` (AU-6): `POST /auth/anonymous`, `result` is
  `ok`, `invalid` (a bad body) or `error` (a 500); `new_account` is `true` for the login
  that created the account
- `auth_refresh_total{result}` (AU-6): `POST /auth/refresh`, `result` is `ok`, `invalid`
  (a bad body, or an unknown or expired token), `revoked` (a logged-out family),
  `reuse_detected` (an exchanged token came back, so its family was revoked) or `error`

Every series exists from start-up at 0, so an alert on `reuse_detected` has a baseline.

**Secrets never reach a log.** No log call takes the request body. The device ID is only
ever hashed (SHA-256) before the store sees it, and refresh tokens are only ever hashed;
the one warn line, `refresh_token_reuse`, carries the account and family IDs, never a
token. `TestLogsNeverCarryDeviceIDsOrTokens` (end to end, Postgres) and
`TestHandlerLogsCarryNoSecrets` / `TestAFailedLoginIsCountedAndLogsNoSecret` (handlers)
log in with a known device ID, refresh, replay a spent token, log out and fail, then
search every log line for the device ID and every token.

`route` is the path part of the **ServeMux pattern** that matched (`/auth/anonymous`), never
the raw URL, so it stays bounded in cardinality and cannot leak identifiers. The method is
already its own label, so the pattern's `POST ` prefix is stripped; the catch-all fallback
is `route="/"`. One JSON access-log line per request carries `method`, `route`, `status`,
`duration_ms` and `request_id`.
Player IDs, staff IDs, raw URLs and request IDs are never metric labels.

---

## Tests

```sh
go test -race ./...
```

The DB-backed tests — `internal/store` and the end-to-end tests in `internal/server` —
need a throwaway Postgres; without it they skip with `AUTH_TEST_DATABASE_URL not set`, so
a plain `go test ./...` stays green. To run them:

```sh
docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=auth_rw \
  -e POSTGRES_DB=auth_test postgres:17
AUTH_TEST_DATABASE_URL='postgres://auth_rw:pw@localhost:5432/auth_test' go test -race ./...
```

They migrate the schema themselves (`internal/testdb`, under a Postgres advisory lock,
because `go test ./...` runs packages in parallel and two packages migrating one fresh
database race) and leave rows behind; point them at a database you do not care about.
Every test uses fresh random keys and IDs, so rerunning against the same database is
fine.

What covers what:

| Test | Covers |
|---|---|
| `internal/server` `TestPlayerLoginEndToEnd` | The whole contract against Postgres: device login and a stable `sub`, the refresh chain, reuse revoking the family, logout, the tokens-only login body (no `services`, D2), every error code and the `405 Allow: POST`s. Access tokens are verified through `MicahParks/keyfunc/v3` (Gateway's JWKS client) against the JWKS the server itself serves, with Gateway's exact parser options. |
| `internal/store` | Queries against the real schema, including 10 concurrent first logins → one account, 8 concurrent refreshes of one token → one rotation, and both races forced deterministically. |
| `internal/api` | Each handler against in-memory fakes: validation, the one-401 rule, log hygiene (no `device_id` or refresh token in logs), opaque 500s, the exact response key set. |
| `internal/token` | Key files, the JWKS shape, and issuance: UUID-only `sub`, the 30 s leeway, a wrong audience refused. |

In CI, `ci/scripts/go-checks.sh` runs the whole suite against the CI Postgres, and the
pre-deploy levels in `ci/services/otomo-auth/unit_test.sh` (sanity, functional,
integration, security, scaling) each run a named slice of it. The DB-backed levels fail
rather than skip when `AUTH_TEST_DATABASE_URL` is unset.

### End-to-end smoke test

For a player login through the gateway and Session, against the compose stack, use
`deploy/scripts/smoke-player-login.sh` (see `deploy/README.md`). The script below
exercises this service's image on its own.

`smoke.sh` exercises the whole lifecycle against the built image: `migrate` twice, `genkey`
(including the refuse-to-overwrite and duplicate-`kid` paths), `serve` with every endpoint,
header and metric checked, a `docker stop` for graceful shutdown, and finally a deactivated
key row to prove `serve` refuses to start. It expects the image `otomo-auth:skeleton`, a
Postgres container named `otomo-pg` on a Docker network named `otomo-test` (user `auth_rw`,
password `pw`, database `auth`), `curl`, and Docker access:

```sh
docker network create otomo-test
docker run -d --name otomo-pg --network otomo-test -e POSTGRES_USER=auth_rw \
  -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=auth postgres:17
docker build -t otomo-auth:skeleton .
sudo sh smoke.sh
```

It binds the service to `127.0.0.1:18080` and `127.0.0.1:19090` while it runs and removes
its own container afterwards; the `otomo-pg` data is left in place.

---

## Deploying

- The image runs as uid `65532` (distroless `nonroot`). The mounted signing key must be
  readable by that uid — `0640` group-readable with a matching group, or simply owned by
  `65532`. A `0600` root-owned key fails at boot with a clear read error.
- Both ports are `EXPOSE`d; only the public one should ever be published.
- `AUTH_ISSUER` and `AUTH_AUDIENCE` must match the Gateway env values character for
  character. A mismatch fails closed — every player request 401s — but it wastes debugging
  time, so change them in both services in the same deploy (contract §9.2).
- **Jenkins note:** `ci/services/Jenkinsfile` runs the container with no environment. With
  fail-fast config it will exit immediately on the missing `AUTH_DATABASE_URL`. That is
  correct and intentional — wiring env and secrets into the deploy is COM-9, not this task.
  Do not add placeholder defaults to make the staging container stay up; it would come up
  "healthy" without a database or a signing key and serve nothing.

---

## Decisions worth not re-litigating

1. **The service learns its own `kid` by looking up the public key**, not from an env var.
   At boot it loads the private key, derives the public half, and finds the matching active
   `signing_key` row; no row means exit `1` with instructions to run `genkey`. This catches
   a key/database mismatch at boot instead of as mysterious 401s at Gateway.
2. **The private key file is PKCS#8 PEM**, mode `0600`.
3. **`genkey` never overwrites**, and removes the file it wrote if the `signing_key` insert
   fails, so operators can retry with the same `-kid`.
4. **Postgres down at boot is exit `1`, with no retry loop.** Gateway retries its JWKS fetch
   because it can still serve public routes; Auth has nothing to serve without its database,
   so failing fast and letting Compose restart is the honest behaviour.
5. **Module path is `github.com/otomo-live/otomo/services/auth`**, not `auth/auth`.
6. **Store tests are gated on `AUTH_TEST_DATABASE_URL`**, not testcontainers — a hard
   dependency on Docker would turn CI red for environmental reasons.
7. **`iss`/`aud` defaults are the contract §9.2 proposed values**, still marked "needs
   confirmation" there. They are env vars, so confirming them later is a config change.
8. **Prometheus uses a private registry**, not the process-wide default one, so a second
   `Server` in a test cannot panic on duplicate registration.
9. **`server.Run` takes a context that is already signal-aware**; `auth.go` builds it with
   `signal.NotifyContext`. Tests cancel that context directly, which exercises the same
   shutdown path without raising a real signal.

---

## Scope

**In:** config, HTTP server and middleware, error shape, metrics, JWKS, signing-key
management, token issuance, device login (AUTH-4), refresh rotation and family
revocation (AUTH-5), the tokens-only login body (AUTH-8, D2), the AUTH-1 schema and its
AUTH-5 indexes.

**Out (do not assume it exists):** key-rotation automation, JWKS rebuild on
`LISTEN/NOTIFY`, multi-key signing, rate limiting (Gateway does that), and the shared
`platform` module (COM-13 — this service deliberately has no imports outside its own
directory, because Jenkins checks out `services/auth` alone).
