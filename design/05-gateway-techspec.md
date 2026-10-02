# Technical Specification — Gateway

**Tasks covered:** GATE-1, GATE-2, GATE-3, GATE-4, GATE-5
**Depends on:** AUTH-2 (Ed25519 JWKS endpoint), AUTH-3 (claim schema — resolved in `06-auth-identity-contract.md`), admin-auth's JWKS endpoint
**Reads:** `00-common-stack.md` §1 (two-service route tables), §1a (network topology), §2.1 (Go conventions)
**Repo location:** `services/gateway/`

This document specifies exactly what to build, not just what the result must do. Where a decision depends on something not yet finalized in AUTH, it's called out in §7 rather than guessed at.

---

## 1. Scope

Gateway is a reverse proxy with authentication middleware. It does not implement business logic.

**Two services, not one binary with two instances.** `services/gateway` is the player edge and `services/gateway_dev` is the admin and dev edge. Each has its own binary, image, config surface and route table, and neither can be talked into serving the other's routes — see `00-common-stack.md` §1 for why this replaced the earlier `GATEWAY_INSTANCE=player|admin` design. Everything below about mechanism (listeners, proxying, JWKS, middleware, metrics) applies to both; the parts that differ are the route table (§5.2), the env prefix (`GATEWAY_DEV_*`), the identity domains a service loads, and whether TLS applies to it. Neither service has CORS: the Godot client is not a browser, and the admin UI is same-origin with `gateway_dev`. Where this document says "the route table", read it as "this service's route table".

**In scope (GATE-1 → GATE-5):**
- HTTP listener, graceful shutdown, health checks
- Reverse proxying to upstream services with per-route configuration
- JWT validation against one or more JWKS sources
- Per-route audience and role enforcement
- Public vs. authenticated route classification, including streaming routes (SSE / long-poll)

**Out of scope for this document:**
- TLS termination details (cert provisioning is an ops task; assume TLS is terminated at Gateway using a cert file path from env, or ask if the school VM has a preferred setup) — **this is no longer purely an ops nicety.** AUTH-8 (see `06-auth-identity-contract.md` §12) requires Gateway's public domain to be fixed and handed to Auth's config, so resolve this before AUTH-8.2 is implemented, not after
- Docker Swarm / multi-VM networking (NET-1, deferred)
- Auth's and admin-auth's own implementations

---

## 2. Repository structure

The current repo has `services/gateway/gateway.go` flat, with `go.mod` and `dockerfile` alongside it. `00-common-stack.md` §3.1 proposed a `cmd/<service>/main.go` + `internal/` layout, which doesn't match what's checked in today.

**Decision for this spec:** keep `gateway.go` at the package root as the entrypoint (matches the existing repo and existing CI Jenkinsfile, which presumably builds `services/<name>`), and add `internal/` packages beside it. Do not introduce a `cmd/` subdirectory for Gateway — that would require confirming the Jenkinsfile's build path first, and there's no benefit here since Gateway is a single binary with no CLI subcommand ambiguity to resolve (unlike Config/Patch/Session, which will want a `migrate` subcommand — Gateway has no database and doesn't need one). **Open question for the common stack doc**: either the template layout in the common doc should be relaxed to match this flat convention, or every other service should adopt `cmd/`. Pick one; don't let them diverge silently.

```
services/gateway/
├── gateway.go              # entrypoint: load config, build routes, start server, wait for signal
├── go.mod
├── dockerfile
├── internal/
│   ├── config/
│   │   └── config.go       # env var parsing into a Config struct
│   ├── router/
│   │   ├── route.go        # Route type, RoleAtLeast helper
│   │   └── player.go       # BuildRoutes() []Route  (the player table)
│   ├── proxy/
│   │   └── proxy.go        # upstream registry, ReverseProxy construction
│   ├── authn/
│   │   ├── jwks.go         # keyfunc registry keyed by issuer name
│   │   └── middleware.go   # token validation + audience/role check
│   ├── server/
│   │   └── server.go       # http.Server wiring, middleware chain, graceful shutdown
│   └── obslog/
│       ├── metrics.go      # Prometheus collectors
│       └── logging.go      # request-scoped slog middleware
└── gateway_test.go
```

---

## 3. Configuration (env vars)

All config is read once at startup into a single struct; fail fast (`log.Fatal`, non-zero exit) if a required var for this service is missing. Nothing is read from env after startup.

`services/gateway/go.mod` pins `go 1.27.1` / `toolchain go1.27.1`, matching `00-common-stack.md` §2.1. The Dockerfile builder stage uses `golang:1.27.1-bookworm` (or the Alpine equivalent, whichever `services/*/dockerfile` already standardizes on).

`services/gateway_dev` reads the same names under a `GATEWAY_DEV_` prefix — `GATEWAY_DEV_LISTEN_ADDR`, `GATEWAY_DEV_METRICS_ADDR`, `GATEWAY_DEV_UPSTREAM_<NAME>_URL`, and so on. **The three staff-domain names are the exception and are shared verbatim**, because they name one domain that both services verify against: `GATEWAY_STAFF_JWKS_URL`, `GATEWAY_STAFF_ISSUER`, `GATEWAY_STAFF_AUDIENCE`. One domain, one set of names, two consumers.

| Variable | Type | Required for | Example | Notes |
|---|---|---|---|---|
| `GATEWAY_LISTEN_ADDR` | string | `gateway` | `:8080` | Public/internal listener |
| `GATEWAY_METRICS_ADDR` | string | `gateway` | `:9090` | `/metrics`, `/healthz`, `/readyz`, pprof — **never** exposed on `GATEWAY_LISTEN_ADDR` |
| `GATEWAY_TLS_CERT_FILE`, `GATEWAY_TLS_KEY_FILE` | path | both, if TLS terminates here | — | See §1 open question |
| `GATEWAY_PLAYER_JWKS_URL` | URL | `gateway` | `http://auth:8080/.well-known/jwks.json` | Auth's JWKS. Required unconditionally: this service exists to serve players, so there is no configuration in which it is optional |
| `GATEWAY_STAFF_JWKS_URL` | URL | both (`gateway` needs it for the Patch dev/staging exception) | `http://admin-auth:8080/.well-known/jwks.json` | admin-auth's JWKS — `gateway_dev`'s only domain |
| `GATEWAY_PLAYER_ISSUER`, `GATEWAY_PLAYER_AUDIENCE` | string | `gateway` | `https://auth.otomo.internal`, `otomo:player` | Expected `iss`/`aud` values, checked in addition to signature |
| `GATEWAY_STAFF_ISSUER`, `GATEWAY_STAFF_AUDIENCE` | string | both | `https://admin-auth.otomo.internal`, `otomo:staff` | Same, staff domain |
| `GATEWAY_UPSTREAM_<NAME>_URL` | URL | per route table | `GATEWAY_UPSTREAM_SESSION_URL=http://session:8080` | One per upstream referenced by this service's route table; §5 lists which |
| `GATEWAY_RATE_LIMIT_RPS`, `GATEWAY_RATE_LIMIT_BURST` | int | both | `20`, `40` | Default per-IP limit; §6.4 covers per-route overrides. **Nothing reads these yet** — see the scope note in `services/gateway/README.md` |
| `GATEWAY_READ_TIMEOUT`, `GATEWAY_WRITE_TIMEOUT`, `GATEWAY_IDLE_TIMEOUT` | duration | both | `10s`, `30s`, `120s` | Server-wide defaults; streaming routes override per-request, see §5.3 |
| `GATEWAY_JWT_CLOCK_SKEW` | duration | both | `30s` | Leeway on `exp`/`nbf`; see §7 note on VM clock sync |

Config validation belongs in `internal/config`, with a unit test asserting that an incomplete env fails to start rather than starting with a nil field that panics on first request. Each service carries its own copy of that test asserting its own minimum.

---

## 4. GATE-1 — HTTP listener and service skeleton

**Server setup (`internal/server`):**
- `http.Server` with `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` from config. `ReadHeaderTimeout` should be short (e.g. 5s) regardless of the others — it's a slowloris mitigation and has no reason to match body-read timeouts.
- Request bodies are capped per route (`Route.MaxBody`, default `router.DefaultMaxBody` = 1 MiB) and an over-cap body is answered 413 `body_too_large`, never proxied. The cap lives in `internal/proxy`, which `gateway` and `gateway_dev` share byte-for-byte, so the player routes get the 1 MiB default too; none of them carries a larger body.
- Graceful shutdown: on `SIGTERM`/`SIGINT`, call `server.Shutdown(ctx)` with a bounded context (e.g. 15s), then exit. In-flight proxied requests get to finish; new ones are refused.
- Two listeners per process: `GATEWAY_LISTEN_ADDR` for proxied traffic, `GATEWAY_METRICS_ADDR` for `/healthz`, `/readyz`, `/metrics`, and `net/http/pprof`. Run them as two `http.Server`s in two goroutines under one `errgroup.Group` so either one's fatal error brings the process down.
- `/healthz`: always 200 once the process is up (liveness only — "is the process alive," not "can it serve traffic").
- `/readyz`: 200 only once every JWKS source this service's route table needs has completed its **first successful fetch**, and all configured upstream URLs have parsed validly. Returns 503 otherwise. Note the honest consequence for `gateway`: it needs two domains, so a admin-auth outage holds it at 503 even though its player routes could serve. Use an `atomic.Bool` set by the JWKS registry's init, not a mutex — this is read on every readiness probe.
- Logging: `slog.NewJSONHandler(os.Stdout, …)`, one line per request via a middleware in `internal/obslog` — method, route pattern (not raw path — see COM-10), status, duration, `group` (public/player/staff), `request_id`. Request ID: read `X-Request-Id` if present (Gateway is usually first hop from an external client so it usually isn't), else generate one (`crypto/rand` or the stdlib `uuid` package, both available in 1.27.1), and set it on the response and on the outbound proxied request per COM-3.

**Acceptance for GATE-1:**
- `go run gateway.go` with a valid env starts, `/healthz` and `/readyz` both reachable on `GATEWAY_METRICS_ADDR`.
- `/readyz` is 503 immediately at startup and flips to 200 within one JWKS fetch cycle.
- `SIGTERM` during an in-flight request lets it complete before the process exits; a new connection attempt during shutdown is refused.
- `/metrics` and pprof are unreachable on `GATEWAY_LISTEN_ADDR`.

---

## 5. GATE-2 — Upstream routing to Essential Services

### 5.1 Route type

```go
package router

type Group int

const (
    GroupPublic Group = iota
    GroupPlayer
    GroupStaff
)

type Role int

const (
    RoleViewer Role = iota + 1
    RoleLiveOps
    RoleAdmin
)

type Route struct {
    Method      string        // "GET", "POST", "*"
    Pattern     string        // net/http.ServeMux pattern: "GET /api/player/session/events"
    Upstream    string        // key into the proxy registry, e.g. "session"
    StripPrefix string        // prefix removed before forwarding; "" = forward as-is
    Group       Group         // which issuer(s) this route accepts — see §6.2
    MinRole     Role          // 0 = no role check (any valid token in Group suffices)
    Stream      bool          // true = SSE/long-poll: disable buffering, extend deadlines (§5.3)
    SetForwarded bool         // gateway_dev only: replace X-Forwarded-For with the peer IP, drop Forwarded/X-Real-Ip, set X-Forwarded-Proto (admin-auth routes)
}
```

`Pattern` uses Go 1.22+ `net/http.ServeMux` syntax directly (method + path + wildcards), so route matching is the standard library's, not custom code.

### 5.2 Route tables

Each service's `internal/router/` exports one `BuildRoutes() []Route` function returning a **Go literal**, not a YAML/JSON file. Route tables change only on redeploy (a new image), so there's no runtime-editing requirement that would justify an external config format — keep it compile-checked and simple, consistent with `00-common-stack.md`'s "avoid unnecessary abstraction."

The two tables below are the specification of *which routes exist*. They now live in two binaries: the first in `services/gateway/internal/router/player.go`, the second in `services/gateway_dev/internal/router/dev.go`.

```go
// services/gateway/internal/router/player.go
func BuildRoutes() []Route {
    return []Route{
        {Method: "*", Pattern: "/auth/", Upstream: "auth", Group: GroupPublic},
        {Method: "GET", Pattern: "/patch/v1/live/manifest", Upstream: "patch", Group: GroupPublic},
        {Method: "GET", Pattern: "/patch/v1/blob/", Upstream: "patch", Group: GroupPublic, Stream: true},
        {Method: "GET", Pattern: "/patch/v1/dev/manifest", Upstream: "patch", Group: GroupStaff},
        {Method: "GET", Pattern: "/patch/v1/staging/manifest", Upstream: "patch", Group: GroupStaff},
        {Method: "*", Pattern: "/api/player/session/events", Upstream: "session", Group: GroupPlayer, Stream: true},
        {Method: "*", Pattern: "/api/player/session/", Upstream: "session", Group: GroupPlayer},
    }
}
```

```go
// services/gateway_dev/internal/router/dev.go
func BuildRoutes() []Route {
    return []Route{
        {Method: "*", Pattern: "/admin-auth/", Upstream: "adminauth", Group: GroupPublic, SetForwarded: true},
        {Method: "*", Pattern: "/admin/", Upstream: "adminui", Group: GroupPublic},
        {Method: "*", Pattern: "/api/admin/config/", Upstream: "config", Group: GroupStaff, MinRole: RoleViewer},
        {Method: "POST", Pattern: "/api/admin/config/channels/live/releases", Upstream: "config", Group: GroupStaff, MinRole: RoleAdmin},
        {Method: "*", Pattern: "/api/admin/dashboard/logs/tail", Upstream: "dashboard", Group: GroupStaff, MinRole: RoleViewer, Stream: true},
        {Method: "*", Pattern: "/api/admin/dashboard/", Upstream: "dashboard", Group: GroupStaff, MinRole: RoleViewer},
        {Method: "*", Pattern: "/api/admin/session/", Upstream: "session", Group: GroupStaff, MinRole: RoleViewer},
        {Method: "*", Pattern: "/api/admin/users", Upstream: "adminauth", Group: GroupStaff, MinRole: RoleAdmin, SetForwarded: true},
        {Method: "*", Pattern: "/api/admin/users/", Upstream: "adminauth", Group: GroupStaff, MinRole: RoleAdmin, SetForwarded: true},
    }
}
```

Two things worth a comment in code: (1) a more specific pattern like `/config/channels/live/releases` must be registered — Go's `ServeMux` already resolves this correctly by pattern specificity, but write a test that asserts it, since silently falling through to a lower role requirement on a route rename is exactly the kind of bug that doesn't show up until it's live. (2) `/admin/` at `GroupPublic` only covers the static asset request; every data call under it goes through `/api/admin/*`, which is separately gated — don't read the public static-file route as "the whole admin app is public."

### 5.3 Reverse proxy construction (`internal/proxy`)

One `*httputil.ReverseProxy` per upstream, built once at startup and reused across requests (this is what gives you connection-pool reuse — building a new proxy per request throws that away):

```go
type Registry struct {
    proxies map[string]*httputil.ReverseProxy
}

func NewRegistry(upstreams map[string]*url.URL) *Registry {
    reg := &Registry{proxies: make(map[string]*httputil.ReverseProxy, len(upstreams))}
    for name, target := range upstreams {
        p := httputil.NewSingleHostReverseProxy(target)
        p.Transport = &http.Transport{
            MaxIdleConnsPerHost: 64,
            IdleConnTimeout:     90 * time.Second,
        }
        p.ErrorHandler = writeUpstreamError // maps to COM-5 error body
        reg.proxies[name] = p
    }
    return reg
}
```

Per-route handling wraps the shared proxy rather than modifying it:

```go
func (reg *Registry) HandlerFor(route Route) http.HandlerFunc {
    p := reg.proxies[route.Upstream]
    return func(w http.ResponseWriter, r *http.Request) {
        if route.StripPrefix != "" {
            r.URL.Path = strings.TrimPrefix(r.URL.Path, route.StripPrefix)
        }
        r.Header.Set("X-Request-Id", requestIDFrom(r.Context()))
        if route.Stream {
            rc := http.NewResponseController(w)
            _ = rc.SetWriteDeadline(time.Time{}) // clear the server-wide write timeout for this response
            streamProxy := *p                     // shallow copy: same Transport, own FlushInterval
            streamProxy.FlushInterval = -1         // flush immediately, don't buffer SSE/long-poll bytes
            streamProxy.ServeHTTP(w, r)
            return
        }
        p.ServeHTTP(w, r)
    }
}
```

The `Stream: true` handling is not optional decoration — without `SetWriteDeadline(time.Time{})`, the server-wide `GATEWAY_WRITE_TIMEOUT` (30s default) will cut Session's long-poll (which can legitimately hold 25s+response time) and Dashboard's log tail mid-stream. Without `FlushInterval = -1`, `httputil.ReverseProxy`'s default buffering holds SSE bytes instead of forwarding them immediately, which breaks the whole point of Dashboard's live tail. This directly implements what SES-E4 and DSH-C10 in the Session and Dashboard specs assumed Gateway would do — build it here, once, rather than re-solving it per route.

**Acceptance for GATE-2:**
- A request to a registered route reaches the correct upstream with the correct path (prefix stripped where configured).
- An unmatched path returns 404 in the COM-5 error shape, not the default `http.NotFoundHandler` text.
- A `Stream: true` route holds a connection open past `GATEWAY_WRITE_TIMEOUT` without being cut.
- Killing an upstream container mid-request surfaces as 502 in the COM-5 shape, not a raw connection-reset to the client.

---

## 6. GATE-3 — JWKS client and JWT validation middleware

### 6.1 Libraries (verified current as of this writing)

| Library | Confirms |
|---|---|
| `github.com/golang-jwt/jwt/v5` | Ships `jwt.SigningMethodEdDSA`, expecting `ed25519.PrivateKey`/`ed25519.PublicKey` — matches AUTH-2's Ed25519 keys directly, no adapter needed |
| `github.com/MicahParks/keyfunc/v3` (backed by `github.com/MicahParks/jwkset`) | Explicitly supports `kty: OKP` / `crv: Ed25519` JWKs (RFC 8037), in addition to EC/RSA. Built for `jwt/v5`. Auto-refreshing via `keyfunc.NewDefaultCtx(ctx, []string{jwksURL})` |

Do not hand-roll JWKS parsing or Ed25519 JWK decoding — both libraries cover this and are maintained specifically for this pairing.

### 6.2 JWKS registry (`internal/authn/jwks.go`)

```go
type Registry struct {
    byGroup map[Group]keyfunc.Keyfunc // GroupPlayer -> player JWKS, GroupStaff -> staff JWKS
}
```

Built at startup from `GATEWAY_PLAYER_JWKS_URL` / `GATEWAY_STAFF_JWKS_URL`, **only for the sources this service's route table actually references** (so `gateway_dev` never fetches the player JWKS: none of its routes are `GroupPlayer`). `keyfunc.NewDefaultCtx` handles background refresh and unknown-`kid` re-fetch on its own; don't build a second polling loop around it.

`/readyz` (§4) waits on this registry's initial fetch for every `Group` referenced by the loaded route table.

### 6.3 Validation middleware (`internal/authn/middleware.go`)

```go
func RequireGroup(reg *Registry, expected map[Group]issuerConfig) func(Route, http.Handler) http.Handler {
    return func(route Route, next http.Handler) http.Handler {
        if route.Group == GroupPublic {
            return next
        }
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            cfg := expected[route.Group]
            kf := reg.byGroup[route.Group]
            tokenStr := bearerToken(r) // strip "Bearer " prefix; missing -> reject below
            if tokenStr == "" {
                writeError(w, r, 401, "missing_token")
                return
            }
            claims := &Claims{}
            token, err := jwt.ParseWithClaims(tokenStr, claims, kf.Keyfunc,
                jwt.WithValidMethods([]string{"EdDSA"}),
                jwt.WithIssuer(cfg.issuer),
                jwt.WithAudience(cfg.audience),
                jwt.WithLeeway(clockSkew),
                jwt.WithExpirationRequired(),
            )
            if err != nil || !token.Valid {
                metricTokenRejected(route.Group, reasonFor(err))
                writeError(w, r, 401, "invalid_token")
                return
            }
            if route.MinRole > 0 && !claims.HasRoleAtLeast(route.MinRole) {
                writeError(w, r, 403, "insufficient_role")
                return
            }
            next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), claims)))
        })
    }
}
```

Critical details, each a specific way this goes wrong if skipped:

- **`jwt.WithValidMethods([]string{"EdDSA"})` is mandatory**, not optional hardening. Without it, `jwt.ParseWithClaims` will accept whatever `alg` the token header claims and ask the keyfunc for a matching key; pinning the algorithm here is what actually prevents an algorithm-substitution attack, not just the key lookup.
- **`iss` and `aud` are checked by parser options, on the token, not inferred from which JWKS answered.** A key existing in the player JWKS says nothing about which issuer or audience the token claims — those are separate, spoofable claims that must be checked explicitly. This is what makes cross-domain rejection (player token on a staff route) actually work, and it's exactly what COM-4's acceptance criteria test.
- **`Claims` struct and `HasRoleAtLeast`** depend on AUTH-3's final claim schema — see §7. Until that's confirmed, code against the proposed shape there and keep the claims struct in its own file so it's a one-place change.
- **Clock skew leeway** (`GATEWAY_JWT_CLOCK_SKEW`, default 30s) exists because VM clocks drift. If M1 is one VM this barely matters; it matters more once NET-1 splits VMs — put an NTP/chrony check on the deployment checklist then, since leeway papers over drift but a large enough drift still breaks `exp` checks in the token's favor or against it unpredictably.

### 6.4 Rate limiting

Per-IP token bucket via `golang.org/x/time/rate`, one `*rate.Limiter` per client IP in a `sync.Map`, swept periodically (e.g. every 5 min, evict entries idle longer than 10 min) so the map doesn't grow unbounded. `GroupPublic` login routes (`/auth/*`, `/admin-auth/*`) get a stricter override (e.g. 5 rps / burst 10) than the general default, since those are the brute-force targets.

**Known M1 limitation, not a blocker:** this limiter is in-memory per Gateway process. With one process per service today that's fine; if either Gateway is ever scaled to multiple replicas, per-IP limits need to move to Valkey (`INCR` + `EXPIRE`) to stay correct across instances. Note it, don't build it now.

**Acceptance for GATE-3 + rate limiting:**
- A syntactically valid, correctly-signed **player** token on a **staff** route → 401.
- A correctly-signed **staff** token missing the required role on an admin-only route → 403.
- A token signed with a key not in the relevant JWKS (e.g. self-signed) → 401.
- A token with `alg` header set to `none` or `HS256` → 401 (rejected by `WithValidMethods` before any key lookup).
- An expired token within `GATEWAY_JWT_CLOCK_SKEW` of `exp` → accepted; a token expired beyond that → 401.
- Exceeding `GATEWAY_RATE_LIMIT_RPS` on `/auth/*` → 429 in the COM-5 shape.

---

## 7. Identity contract — now resolved

§6.3's `Claims` struct and the claim-checking values (`iss`/`aud`/`roles` shape) are pinned in **`06-auth-identity-contract.md`**, not guessed at here anymore. That document also resolves the algorithm question for admin-auth (§2 there: EdDSA for both domains, not just Auth) and gives a fixture generator (§10) that lets the acceptance tests in §6.3/§8 below be written and passing before Auth or admin-auth are running.

One item remains genuinely open and is *not* resolved: **AUTH-8, "next-hop hand-off payload."** `06-auth-identity-contract.md` §7 covers what's known. Until it's clarified, this spec's route tables (§5.2) assume AUTH-8 has no effect on Gateway routing.

## 8. GATE-4 — Token scope/audience enforcement

Already specified in §6.3 (the `iss`/`aud` parser options and `MinRole` check) — GATE-4 is not a separate code path from GATE-3, it's the claims-checking half of the same middleware. Listed as its own task ID because it has its own acceptance criteria, not because it's built separately:

**Acceptance for GATE-4 (superset of the token-validity cases in §6.3):**
- `aud` present and correctly signed but not equal to the route's expected audience → 401, distinct log/metric reason (`aud_mismatch`) from a bad signature, so this is debuggable in the Dashboard without reading raw logs.
- Role hierarchy is respected: a `RoleAdmin` token passes a `MinRole: RoleLiveOps` check; a `RoleViewer` token fails a `MinRole: RoleLiveOps` check.
- `gateway_token_rejected_total{reason,group}` (per `00-common-stack.md`'s Dashboard doc naming) increments correctly for each rejection reason: `missing_token`, `invalid_signature`, `expired`, `aud_mismatch`, `iss_mismatch`, `insufficient_role`. **Both services emit this same series on purpose.** COM-10's prefix rule exists so two services emitting one name cannot be confused, and here they cannot: `group` is `staff` on every route `gateway_dev` protects, and the `route` label holds `ServeMux` patterns that appear in one table only, so no label value can come from both. One series means one dashboard question — "which edge is rejecting tokens, and why" — instead of two. Do not rename it without changing `01-dashboard.md` DSH-B5 in the same commit.

---

## 9. GATE-5 — Route policy: public vs. authenticated

This is the `Group` field on every `Route` entry (§5.1/§5.2) plus the rate-limit differences already covered in §6.4. There's no additional code surface beyond what GATE-2/GATE-3 already build — GATE-5's job is making sure the route tables in §5.2 are **correct and reviewed**, not writing new mechanism.

**Explicit policy, so it's checked rather than assumed:**
- Public: `/auth/*`, `/admin-auth/*` (both are how you *get* a token, so they can't require one), `/admin/*` static assets, `/patch/v1/live/*`.
- Player: `/api/player/session/*`.
- Staff: `/api/admin/*`, `/patch/v1/dev/*`, `/patch/v1/staging/*`.
- Auth's and admin-auth's own JWKS endpoints (`/.well-known/jwks.json`) are **never proxied through Gateway at all** — Gateway (and any other backend service that verifies tokens) fetches them directly over the internal network as a client, not as a routed request. No client ever verifies a JWT itself in this architecture, so there's no reason to expose either JWKS endpoint publicly.

**Acceptance for GATE-5:**
- Every route in both route tables has a `Group` reviewed against this list by someone other than whoever wrote the table (pair or PR review, not a solo check). A group is a property of the route, not of the service it happens to live in: `gateway` carries two `GroupStaff` routes, and `gateway_dev` carries two `GroupPublic` ones.
- A test enumerates every `Route` in each service's `BuildRoutes()` and asserts none has `Group: GroupPublic` under `/api/` (the one path prefix that should never be reachable without a token, in either service) — this is a cheap regression guard against a future route being added with the wrong default. Note what it must *not* catch: `/admin-auth/*` and `/admin/*` are legitimately public in `gateway_dev`, which is why the invariant is scoped to `/api/` and not to "every route".
- Startup verification asserts that every protected route's group has both a JWKS URL and an expected issuer/audience (`verifyIssuers` in each service's entrypoint), and that every route's upstream resolves in the proxy registry (`verifyUpstreams`). Both are exit-worthy: a route with no key source would start and then reject everything, which is harder to diagnose than a process that refuses to start. The domains loaded are derived from `router.UsedGroups(routes)` rather than listed a second time, so a table edit cannot add a domain the process does not fetch.

---

## 9a. GATE-6 — Public endpoint publication

Small, but blocking for AUTH-8.2. No proxying or validation code changes — this is a configuration/documentation task.

- Decide and fix the externally reachable scheme and host for `gateway`. **Decided:** it is `OTOMO_PUBLIC_BASE_URL` in `deploy/.env`, for example `https://play.example.com`.
- Record it in `06-auth-identity-contract.md` §12 once decided. The client SDK is configured with it; Auth no longer needs it (D2 removed the `services` hand-off).
- Confirm the public path for Session stays `{base}/api/player/session`, matching the route already in §5.2 below — don't let this drift out of sync with the actual `Pattern` value in `router/player.go`.
- No route exists yet for `{base}/api/player/match`: do not add one this milestone. The path is reserved in the identity contract doc (§12).

**Acceptance:** the value in `06-auth-identity-contract.md` §12 is filled in (not `TBD`), and it matches what `GATEWAY_TLS_CERT_FILE`/`GATEWAY_TLS_KEY_FILE` (§3) actually serve.

---

## 10. Testing

| Layer | Tool | What |
|---|---|---|
| Unit | `go test -race` | Route matching, `HasRoleAtLeast` ordinal logic, config validation (§3), claims parsing against fixture tokens |
| Unit (crypto) | `go test`, `crypto/ed25519`, `httptest.NewServer` | Generate an ephemeral Ed25519 keypair, serve a hand-built JWKS JSON from an `httptest.Server`, sign test tokens with `jwt/v5`, assert accept/reject per the §6.3 and §8 acceptance lists — this is the test suite that actually proves the middleware works, independent of Auth being up |
| Contract | Hurl, run in Jenkins against a compose test stack with Auth/admin-auth stubs | Every case in §6.3, §8, §9 acceptance lists, executed as real HTTP requests through the running binary |
| Load | k6 (stretch, not required for M1 acceptance) | Concurrent long-poll/SSE connections held open past `GATEWAY_WRITE_TIMEOUT`, confirming §5.3's deadline-clearing actually holds under load and not just in a single manual test |

---

## 11. Definition of done

- `gateway` and `gateway_dev` are separate services with separate images (`otomo-gateway`, `otomo-gateway_dev`), separate CI jobs and separate Compose services. Neither can be reconfigured into serving the other's routes: the admin route table is not present in `gateway`'s binary, and the player route table is not present in `gateway_dev`'s.
- `gateway_dev` is not published to the internet; if the school VM's networking cannot enforce that, it is reached through an IP allowlist or VPN and the fact is recorded as a known deviation rather than assumed away.
- Every acceptance criterion in §4, §5, §6, §8, §9 passes in Jenkins.
- §7's open questions have answers, and the `Claims` struct and route tables reflect them (or this spec has been revised if the answers change the shape).
- A player token cannot reach any `/api/admin/*` or `/patch/v1/{dev,staging}/*` route; a staff token cannot reach `/api/player/*`; both fail closed (401), not silently pass through.
- Session's long-poll and Dashboard's log tail both survive longer than `GATEWAY_WRITE_TIMEOUT` when proxied through Gateway.
