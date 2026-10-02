# gateway

The public player gateway: the HTTP front door for the player surfaces
(`/auth/*`, the Patch manifests and blobs, and `/api/player/session/*`).

It is a **separate service from `services/gateway_dev`**, not a second instance
of it. `services/gateway` serves players and knows only the player identity
domain (plus the staff domain for the two dev/staging patch manifests);
`gateway_dev` serves staff. Neither can reach the other's routes, and neither
can be configured into the other's identity provider.

Source of truth: `design/05-gateway-techspec.md` (routing, config, middleware),
`design/06-auth-identity-contract.md` (the claim shape and issuer),
`design/00-common-stack.md` (service conventions). The route table is recorded in
`ROUTE-POLICY.md`.

**What exists today:** config, both listeners, request ID, structured logging,
metrics, JWKS fetching with readiness, the auth middleware, per-client-IP rate
limiting, upstream proxying with streaming, and client-IP resolution behind a
trusted reverse proxy.

---

## Running it

```sh
set -a; . ./.env; set +a   # or export the vars yourself
go run .
```

`.env.example` lists every variable with a working local value. The process
exits `1` at boot rather than starting half-configured: a missing upstream for
any route, a protected route whose domain has no JWKS URL or no expected
issuer/audience, or an unparseable address is exit-worthy.

---

## Configuration

Every variable is read once at startup into `internal/config.Config`. See
`.env.example`.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `GATEWAY_LISTEN_ADDR` | **yes** | — | Public listener. |
| `GATEWAY_METRICS_ADDR` | **yes** | — | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed. |
| `GATEWAY_PLAYER_JWKS_URL` | **yes** | — | Auth's JWKS. Must be an absolute URL, checked at boot. |
| `GATEWAY_PLAYER_ISSUER` / `GATEWAY_PLAYER_AUDIENCE` | **yes** | — | `https://auth.otomo.internal` / `otomo:player`. |
| `GATEWAY_STAFF_JWKS_URL` | **yes** | — | admin-auth's JWKS, for the two dev/staging patch manifest routes. |
| `GATEWAY_STAFF_ISSUER` / `GATEWAY_STAFF_AUDIENCE` | **yes** | — | `https://admin-auth.otomo.internal` / `otomo:staff`. |
| `GATEWAY_UPSTREAM_<NAME>_URL` | **yes**, per route | — | `AUTH`, `PATCH`, `SESSION`. The name is lowercased into the route's `Upstream` key. |
| `GATEWAY_TRUSTED_PROXIES` | no | empty | Comma-separated CIDRs or bare IPs (a bare IP becomes a `/32` or `/128`); whitespace-tolerant. Peers in the list may name their client in `X-Forwarded-For`. See below. |
| `GATEWAY_TLS_CERT_FILE` / `GATEWAY_TLS_KEY_FILE` | no | — | Both or neither. |
| `GATEWAY_READ_TIMEOUT` | no | `10s` | |
| `GATEWAY_WRITE_TIMEOUT` | no | `30s` | Superseded per-request on the `Stream` routes. |
| `GATEWAY_IDLE_TIMEOUT` | no | `120s` | |
| `GATEWAY_JWT_CLOCK_SKEW` | no | `30s` | Leeway on `exp`/`nbf`. |
| `GATEWAY_RATE_LIMIT_RPS` / `GATEWAY_RATE_LIMIT_BURST` | no | `20` / `40` | General per-client-IP limit. |
| `GATEWAY_LOGIN_RATE_LIMIT_RPS` / `GATEWAY_LOGIN_RATE_LIMIT_BURST` | no | `5` / `10` | Stricter per-client-IP limit for `/auth/`. |
| `GATEWAY_SWEEP_INTERVAL` | no | `5m` | How often idle per-client-IP buckets are evicted. |
| `GATEWAY_MAX_IDLE_AGE` | no | `10m` | Buckets idle longer than this are evicted. |
| `GATEWAY_LOG_PRETTY` | no | — | `1` for human-readable text. Local development only. |

An invalid `GATEWAY_TRUSTED_PROXIES` entry fails the boot with the offending
entry named in the error.

---

## Client IPs behind the edge

TLS is terminated by a reverse proxy (the edge) at a fixed address. Without
configuration the gateway keys its per-IP rate limit on `r.RemoteAddr`, which
is the edge for every player, so the whole game would share one bucket.

Set `GATEWAY_TRUSTED_PROXIES` to the edge's address (for example its compose
service IP or the docker subnet). The `internal/clientip` middleware runs
outermost, before request ID, logging and recover:

- The peer IP is parsed from `r.RemoteAddr`. If it is **not** inside a trusted
  prefix, nothing changes: a client-supplied `X-Forwarded-For` is ignored,
  exactly as when the setting is empty.
- If the peer is trusted and `X-Forwarded-For` is present, the **rightmost**
  non-empty entry is parsed as the client. The request continues with
  `RemoteAddr` set to that client and the header **deleted**, so the rate
  limiter, the access log's `client_ip` field and the proxy's forwarding logic
  all use the resolved client and nothing downstream re-trusts the inbound
  header. The rightmost entry is the one written by the nearest trusted hop;
  entries to its left may have been supplied by the client.
- If the entry does not parse, the peer address is kept and the header is still
  deleted.

The edge is expected to **overwrite** `X-Forwarded-For` with exactly the client
address, but the rightmost rule stays correct if the header is a chain. Leave
`GATEWAY_TRUSTED_PROXIES` empty when the gateway is reached directly (an SSH
tunnel, a local run): trust nobody, and every request is keyed on its true peer.

---

## HTTP surface

| Method | Pattern | Upstream | Group | Rate limit |
|---|---|---|---|---|
| `*` | `/auth/` | auth | public | login |
| `GET` | `/patch/v1/live/manifest` | patch | public | general |
| `GET` | `/patch/v1/blob/` | patch | public | general, streaming |
| `GET` | `/patch/v1/dev/manifest` | patch | staff | general |
| `GET` | `/patch/v1/staging/manifest` | patch | staff | general |
| `*` | `/api/player/session/events` | session | player | general, streaming |
| `*` | `/api/player/session/` | session | player | general |
| anything else | — | — | — | COM-5 `404` |

Middleware, outermost first: client IP → request ID → access log → recover →
per-route rate limit → auth → mux. The limiter runs before auth so a flood of
bad tokens is turned away before any signature check.

Request bodies are capped at 1 MiB (`router.DefaultMaxBody`); an over-cap body
is answered `413 body_too_large` and never proxied. The `/patch/v1/blob/` and
`/api/player/session/events` routes clear the write deadline per request so a
slow download or a long poll is not cut off by `GATEWAY_WRITE_TIMEOUT`.

### Errors (COM-5)

```json
{ "error": { "code": "not_found", "message": "...", "request_id": "..." } }
```

---

## Observability

One JSON access log line per request, including `method`, `client_ip` (the
resolved client, not the edge), `route` (the ServeMux pattern path, never the
raw URL), `status`, `duration`, `group`, `reason` and `request_id`.

- `gateway_http_request_duration_seconds{route,method,status}`
- `gateway_token_rejected_total{reason,group}`

The `reason`/`group` labels are outside COM-10's allowed set and techspec §8
requires them; the TODO in `internal/obslog/metrics.go` records the conflict.

---

## Internal listener

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200`. |
| `GET /readyz` | `200` once every JWKS source the route table uses has been fetched and parsed; `503` until then. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

None of these exist on the public listener.

---

## Scope

**In:** config, both listeners, request ID, structured logs with `client_ip`,
metrics, JWKS fetching with readiness, the auth middleware, per-client-IP rate
limiting, client-IP resolution behind a trusted proxy, request-body caps,
upstream proxying with streaming, the route table.

**Out (do not assume it exists):** upstream reachability in `/readyz`, retries
or circuit breaking, audit logging, CORS (the Godot client is not a browser),
and the shared `platform` module (COM-13).
