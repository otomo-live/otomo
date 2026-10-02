# gateway_dev

The admin and dev edge: the HTTP front door for the staff surfaces (`/api/admin/*`),
the admin UI (`/admin/`) and admin-auth (`/admin-auth/`).

It is a **separate service from `services/gateway`**, not a second instance of it.
`services/gateway` serves players and knows only the player identity domain;
`gateway_dev` serves staff and knows only the staff domain. Neither can reach the
other's routes, and neither can be configured into the other's identity provider.

Source of truth: `design/05-gateway-techspec.md` (routing, config, middleware),
`design/06-auth-identity-contract.md` §5 (the staff claim shape and issuer),
`design/00-common-stack.md` (service conventions).

**What exists today:** config, listeners, request ID, logging, metrics, JWKS
fetching with readiness, the auth middleware, per-IP rate limiting (`§6.4`), and
upstream proxying. There is no upstream health in `/readyz`, and no CORS
middleware because the admin UI is same-origin; see "Scope".

---

## Why a service and not `GATEWAY_INSTANCE=admin`

`design/00-common-stack.md` §1 originally specified one image deployed as two
containers, distinguished by a `GATEWAY_INSTANCE` env var. This service overrides
that, deliberately, for three reasons.

1. **Two identity providers, two audiences.** The two sides do not share a
   verifier. A player token is Ed25519-signed by `services/auth` with
   `aud: otomo:player` and no `roles` claim; a staff token is signed by
   admin-auth with `aud: otomo:staff` and a `roles` array. The flag design meant the
   player-facing container was configured with both, including the staff JWKS URL
   and both issuers.
2. **A flag is not a boundary.** Under the instance design, whether admin routes
   were reachable was one environment variable on a container facing the public
   internet. A typo, a copy-pasted deploy, or a `docker run` without `-e` changed
   the answer. Here the admin route table is not in the player binary at all, so
   there is nothing to misconfigure: `services/gateway` returns `404` for
   `/api/admin/...` and `gateway_dev` returns `404` for `/api/player/...`, and both
   are asserted by tests.
3. **Blast radius.** The split is described in the common stack as load isolation
   and blast radius, not a code fork. A fork serves that better: the public
   container no longer contains the staff route table, the admin upstreams, or the
   staff key source.

The cost is duplication, and it is bounded and checked. See below.

## Relationship to `services/gateway`

`internal/{apierr,authn,obslog,proxy,server}`, `internal/router/route.go` and the
test fixture are **copies** of `services/gateway`'s, byte-identical apart from the
module path. Only three files genuinely differ:

| File | Difference |
|---|---|
| `internal/router/dev.go` | this service's route table (`BuildRoutes`) |
| `internal/config/config.go` | no `Instance`, no player domain, `GATEWAY_DEV_*` prefix |
| `gateway_dev.go` | no route-table switch |

Copies rather than a shared module because of the build context:
`ci/services/Jenkinsfile` runs `docker build ... services/${params.SERVICE}`, so a
sibling module is not in the Docker context at all, and every adopter's
`COPY go.mod go.sum ./ && go mod download` layer would have to be widened and lose
its cache. The same reasoning already applies to `services/auth` and
`services/session`, which each carry their own JWKS verifier rather than importing
one.

The real fix is the shared `platform` module (COM-13), which is out of scope here
and needs that build-context change first. Until then the copies are held together
by `diff`: keep the files above in sync, and if you change one of the identical
files, change both.

**The one deliberate asymmetry:** `internal/obslog/metrics.go` keeps the
`gateway_*` metric names rather than taking a `gateway_dev_` prefix. Every label
value is disjoint between the two services (`group` is `staff` here, `player` or
`staff` there; the `route` patterns do not overlap), so the series cannot be
confused, and DSH-B5 plus techspec §8 both name `gateway_token_rejected_total`.
The file carries a comment saying so.

---

## Running it

```sh
export GATEWAY_DEV_LISTEN_ADDR=:8080
export GATEWAY_DEV_METRICS_ADDR=:9090
export GATEWAY_DEV_UPSTREAM_ADMINAUTH_URL=http://127.0.0.1:9004
export GATEWAY_DEV_UPSTREAM_ADMINUI_URL=http://127.0.0.1:9005
export GATEWAY_DEV_UPSTREAM_CONFIG_URL=http://127.0.0.1:9006
export GATEWAY_DEV_UPSTREAM_DASHBOARD_URL=http://127.0.0.1:9007
export GATEWAY_DEV_UPSTREAM_SESSION_URL=http://127.0.0.1:9003
export GATEWAY_STAFF_JWKS_URL=http://localhost:9999/.well-known/staff-jwks.json
export GATEWAY_STAFF_ISSUER=https://admin-auth.otomo.internal
export GATEWAY_STAFF_AUDIENCE=otomo:staff

go run .
```

`testdata/manual/` has a fake upstream per route and `run-dev.ps1`, which sets all
of the above; `testdata/manual/TESTING.md` walks the checks, including the ones that
should fail.

The process exits `1` at boot rather than starting half-configured: a missing
upstream for any route, or a protected route whose domain has no JWKS URL or no
expected issuer/audience, is exit-worthy. So is an unparseable
`GATEWAY_STAFF_JWKS_URL`, checked at boot because once the process is running a
typo there is indistinguishable from admin-auth being down.

---

## Configuration

Every variable is read once at startup. See `.env.example`.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `GATEWAY_DEV_LISTEN_ADDR` | **yes** | — | Public listener. Must not be internet-reachable. |
| `GATEWAY_DEV_METRICS_ADDR` | **yes** | — | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed. |
| `GATEWAY_DEV_UPSTREAM_<NAME>_URL` | **yes**, per route | — | `ADMINAUTH`, `ADMINUI`, `CONFIG`, `DASHBOARD`, `SESSION`. Name is lowercased into the route's `Upstream` key. |
| `GATEWAY_STAFF_JWKS_URL` | **yes** | — | admin-auth's JWKS. Must be an absolute URL. |
| `GATEWAY_STAFF_ISSUER` | **yes** | — | `https://admin-auth.otomo.internal` in the contract. |
| `GATEWAY_STAFF_AUDIENCE` | **yes** | — | `otomo:staff`. |
| `GATEWAY_DEV_TLS_CERT_FILE` / `_KEY_FILE` | no | — | Both or neither. |
| `GATEWAY_DEV_READ_TIMEOUT` | no | `10s` | Cleared per-request on `Upload` routes. |
| `GATEWAY_DEV_WRITE_TIMEOUT` | no | `30s` | Superseded per-request on `Stream` and `Upload` routes. |
| `GATEWAY_DEV_IDLE_TIMEOUT` | no | `120s` | |
| `GATEWAY_DEV_JWT_CLOCK_SKEW` | no | `30s` | Leeway on `exp`/`nbf`. |
| `GATEWAY_DEV_RATE_LIMIT_RPS` / `_BURST` | no | `20` / `40` | General per-IP limit, every route except the two buckets below. |
| `GATEWAY_DEV_LOGIN_RATE_LIMIT_RPS` / `_BURST` | no | `5` / `10` | Stricter per-IP limit for `/admin-auth/`. |
| `GATEWAY_DEV_SWEEP_INTERVAL` | no | `5m` | How often idle per-IP entries are evicted. |
| `GATEWAY_DEV_MAX_IDLE_AGE` | no | `10m` | Entries idle longer than this are evicted. |
| `GATEWAY_DEV_LOG_PRETTY` | no | — | `1` for human-readable text. Local dev only. |

Note the three `GATEWAY_STAFF_*` names carry no `GATEWAY_DEV_` prefix. That is
intentional: the contract fixes them, and `services/gateway` reads the same three
for the two patch manifest routes that cross into the staff domain. One domain, one
set of names, two consumers.

**A consequence worth knowing:** `GATEWAY_DEV_UPSTREAM_SESSION_URL` points at the
same Session deployment `services/gateway` proxies to `/api/player/session/`. One
Session, two gateways, two prefixes, two identity domains checked at the edge.

---

## HTTP surface

### Public listener

| Route | Upstream | Group | Minimum role |
|---|---|---|---|
| `/admin-auth/` | adminauth | public | — |
| `/admin/` | adminui | public | — |
| `/api/admin/session/` | session | staff | `viewer` |
| `/api/admin/dashboard/` | dashboard | staff | `viewer` |
| `/api/admin/dashboard/logs/tail` | dashboard | staff | `viewer`, streaming |
| `/api/admin/config/` | config | staff | `viewer` |
| `POST /api/admin/config/channels/live/releases` | config | staff | `admin` |
| `POST /api/admin/config/packs` | config | staff | `live_ops`, 512 MiB upload |
| `/api/admin/users` and `/api/admin/users/` | adminauth | staff | `admin` |
| anything else | — | — | COM-5 `404` |

`/admin-auth/` and `/admin/` are unauthenticated at this layer because they are the
login surface and the static admin bundle; admin-auth authenticates the first
and the bundle is not secret. That is exactly why this listener must not be
internet-reachable (common-stack §1): bind it to a private network, a VPN, or an IP
allowlist.

The `POST .../channels/live/releases` row is more specific than the
`/api/admin/config/` prefix, so `net/http.ServeMux` resolves it first and a
`live_ops` token cannot publish a release. `TestServeMuxSpecificity_SpecificRouteWins`
and `TestRoutePolicyUnderRealMiddleware` both hold that. `POST
/api/admin/config/packs` is likewise more specific than the prefix; it sits at
`live_ops` because that matches Config's own bar for a pack upload.

Request bodies are capped at 1 MiB (`DefaultMaxBody`) on every route unless the
route sets `MaxBody`. A request whose `Content-Length` exceeds the cap is refused
with `413` and code `body_too_large` before the upstream is touched; a chunked
body with no `Content-Length` is caught mid-copy by `http.MaxBytesReader` and
surfaces through the proxy's error handler as the same `413`. The packs route
raises the cap to 512 MiB and sets `Upload`, which clears *both* the read and
write deadlines with `http.NewResponseController`: net/http's `WriteTimeout`
runs from the end of the request headers, so a 512 MiB upload would otherwise be
killed by the read timeout while the client is still sending and by the write
timeout before the upstream could answer.

The config prefix is `viewer` because Config enforces its own per-route roles
(reads `viewer`, writes `live_ops` or `admin`); a stricter edge would refuse every
read a viewer makes before Config sees it. `/api/admin/users` is registered both as
the exact path and as a subtree: with the subtree alone, ServeMux answers the
collection root with a `307` to `/api/admin/users/` before auth runs, so an
anonymous caller would learn the route exists instead of getting a `401`.

Per-IP rate limiting (techspec §6.4) splits the table three ways. `/admin/`, the
static SPA bundle, has **no** limiter: one page load fetches dozens of hashed
assets at once, so a per-IP bucket there breaks the UI rather than stopping
abuse. `/admin-auth/`, the staff login, refresh, logout, mfa and onboard surface,
uses the stricter **login** bucket — it is the brute-force target. Every other
route, `/api/admin/*` included, uses the **general** bucket. The limiter runs
before auth, so a flood of junk tokens is turned away before any signature check,
and the `/` catch-all 404 stays unlimited: a 404 costs nothing and limiting it
would only map the surface. Limits are per `RemoteAddr`; `X-Forwarded-For` is
never trusted.

Behind the SSH tunnel every admin arrives from the same peer address (the Docker
bridge), so in practice these buckets are shared by the whole team, not per person.
That is acceptable because only people with SSH access reach this listener at all;
per-account lockout in admin-auth is what stops password guessing.

The `/admin-auth/`, `/api/admin/users` and `/api/admin/users/` routes set
`SetForwarded`: admin-auth rate-limits and logs per client, so the proxy deletes any
client-supplied chain (and any `Forwarded` / `X-Real-Ip`) and forwards exactly the
peer IP, plus `X-Forwarded-Proto`.
Every other route keeps the appended chain (`<client-supplied>, <peer ip>`) with no
proto; upstreams that do not need the true client must not trust it.

Middleware, outermost first: request ID → access log → recover →
per-route rate limit → auth → mux.

### Internal listener

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200`. |
| `GET /readyz` | `200` once the staff JWKS has been fetched and parsed; `503` until then. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

None of these exist on the public listener. `/readyz` waits on the JWKS only, not
on the upstreams being reachable (techspec asks for both; the upstream half is not
implemented).

### Errors (COM-5)

```json
{ "error": { "code": "not_found", "message": "...", "request_id": "..." } }
```

### Authentication

A protected route needs `Authorization: Bearer <staff token>`. The middleware
validates in a fixed order (`kid` → signature → issuer → audience; algorithm pinned
to `EdDSA`; `exp` required; `GATEWAY_DEV_JWT_CLOCK_SKEW` leeway) and answers:

| | Meaning | Codes |
|---|---|---|
| `401` | The token is unusable. The client should get a new one. | `missing_token`, `invalid_token`, `invalid_signature`, `expired`, `iss_mismatch`, `aud_mismatch` |
| `403` | The token is valid and correctly issued, but the subject lacks the required role. | `insufficient_role` |

Every protected route is `GroupStaff`, so the only issuer this service will ever
accept is admin-auth's. A player token fails on `invalid_signature` rather than
`iss_mismatch`: the player key is not in this registry, so verification never gets
as far as comparing issuers. The `code` is both the COM-5 `code` and the `reason`
label on `gateway_token_rejected_total`.

A protected route whose domain has no key source fails closed with `500
internal_error`; `verifyIssuers` in `main` makes that unreachable by refusing to
start.

### Metrics and logs (COM-10)

- `gateway_http_request_duration_seconds{route,method,status}`
- `gateway_token_rejected_total{reason,group}`

`route` is the ServeMux pattern's path, never the raw URL, so cardinality stays
bounded. Staff IDs, player IDs, raw URLs and request IDs are never labels. The
`reason`/`group` labels are outside COM-10's allowed set (`route`, `method`,
`status`, `channel`) and techspec §8 requires them; the existing TODO in
`internal/obslog/metrics.go` records the conflict rather than hiding it.

---

## Deploying

- Image `otomo-gateway_dev:staging`, distroless, uid `65532`.
- **Do not publish the public listener to the internet.** It is the staff control
  plane, and `/admin-auth/` is an unauthenticated login endpoint on it.
- Both images are separate Jenkins jobs (`otomo-gateway`, `otomo-gateway_dev`) and
  separate ports in `ci/dispatcher/Jenkinsfile`.
- `GATEWAY_STAFF_ISSUER` and `GATEWAY_STAFF_AUDIENCE` must match what admin-auth
  actually puts in its tokens, character for character. A mismatch fails closed:
  every protected request 401s.
- The container runs with no environment under `ci/services/Jenkinsfile`, so it exits
  immediately on the missing required vars. That is correct; wiring env and secrets is
  COM-9. Do not add placeholder defaults to keep it up.

---

## Decisions worth not re-litigating

1. **A separate service, not a second instance.** See the top of this file. The
   specs that said otherwise are updated in the same change.
2. **`GroupPlayer` stays in `internal/router/route.go`** even though no route uses
   it. Deleting it would take the cross-domain middleware tests with it, and those
   tests are the proof that a player token is rejected here.
3. **Metric names stay `gateway_*`.** See the metrics note above.
4. **The `service` key in the startup log line is gone** along with the instance
   field; the binary's name is in the message.
5. **`jwksSources` keeps its one-case switch** so it stays diffable against
   `services/gateway`'s; the shape is deliberate.
6. **Config validation is a plain list of checks in `validate()`**, not a
   per-instance table, because there is one instance.
7. **`/api/admin/session/` is proxied here, not in `services/session`.** The service
   owns the handler; this service owns who may reach it.

---

## Scope

**In:** config, both listeners, request ID, structured logs, metrics, JWKS fetching
with readiness, the staff auth middleware and its role bar, per-IP rate limiting
(general, login and unlimited buckets), request-body caps (1 MiB default, 512 MiB
packs upload), upstream proxying with streaming support, the route table.

**Out (do not assume it exists):** upstream reachability in `/readyz`, retries or
circuit breaking, audit logging (that is Session's
`/api/admin/session/audit`), and the shared `platform` module (COM-13).
