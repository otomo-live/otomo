# GATE-1 Reference for GATE-2

This file summarises what GATE-1 built so a new conversation can pick up GATE-2 without re-reading the full history.

> **Historical snapshot, partially superseded.** This was written between GATE-1
> and GATE-2. Two later changes invalidate parts of it, and the deltas are marked
> inline below rather than silently rewritten, so the record of what GATE-1
> actually shipped stays readable:
>
> - **GATE-3** (SCRUM-? JWKS-backed authentication middleware) added
>   `internal/authn/{jwks,claims,middleware}.go`, the `Claims` struct, and the
>   route-table guards in `internal/authn/middleware_test.go`. `/readyz` now waits
>   on the JWKS registry, so the last open item below is closed.
> - A later change split Gateway into two services: `services/gateway` (players)
>   and `services/gateway_dev` (admins and devs). `GATEWAY_INSTANCE`, the
>   `Instance` type and `BuildAdminRoutes()` are gone; see
>   `00-common-stack.md` §1 and `services/gateway_dev/README.md`. This file
>   describes `services/gateway` as it is today, except where a section says
>   otherwise.

---

## Authoritative specs (read these first)

- `design/05-gateway-techspec.md` -- gateway techspec (GATE-2 is §5)
- `design/06-auth-identity-contract.md` -- JWT claims shape
- `design/00-common-stack.md` -- shared conventions (COM-*)

## Hard constraints

- Go **1.27.1** exactly, stdlib `net/http` and `ServeMux`
- Spec code blocks are intended implementation, not illustrations. Do not substitute
  libraries, restructure layout, or improve on specified approaches.
- stdlib `uuid` package (`uuid.NewV7()` for time-ordered UUIDs)
- `log/slog` JSON handler on stdout
- `prometheus/client_golang` for metrics
- `golang-jwt/jwt/v5` + `MicahParks/keyfunc/v3` for JWT (GATE-3, not GATE-2)
- COM-5 error shape: `{"error":{"code","message","request_id"}}`
- COM-10 metric naming: `<service>_<thing>_<unit>`
- COM-11 Dockerfile: `golang:1.27.1-bookworm` builder, `gcr.io/distroless/static-debian12:nonroot` runtime
- pprof on separate internal metrics port
- British English in comments and docs, no em-dashes
- ~~User self-pushes via GitHub Desktop (do not `git push`)~~ — **superseded.**
  This was a GATE-1-era arrangement. The standing instruction for the service work
  is the opposite: push the branch when the work lands. Note that a Windows
  `git push` hangs on the credential-manager prompt in this environment, so push
  from WSL over SSH (`github-push-via-wsl-ssh`).

## Module and Go mod

- Module path: `gateway/gateway`
- Direct dependencies: `prometheus/client_golang v1.22.0`, `golang.org/x/sync v0.14.0`
- GATE-2 will likely need `net/http/httputil` (stdlib, no go.mod change)

## Project layout after GATE-1

```
services/gateway/
  .gitattributes          -- LF line endings for CI files (COM-1)
  dockerfile              -- two-stage distroless build (COM-11)
  go.mod / go.sum
  gateway.go              -- entrypoint: Init slog, Load config, build routes, register, Run
  gateway_test.go         -- integration tests (the GATE-1 count is not repeated
                             here; it has grown since and a stale number is worse
                             than none)
  verify_upstreams_test.go -- internal tests for verifyUpstreams
  internal/
    apierr/
      apierr.go           -- COM-5 error writer + request-ID context helpers
    config/
      config.go           -- all GATEWAY_* env vars, parsed once at startup
      config_test.go       -- table-driven config tests
    obslog/
      logging.go          -- slog init, RequestIDMiddleware, LoggingMiddleware,
                             RecoverMiddleware, statusWriter (with Unwrap)
      metrics.go          -- Prometheus histogram + counter (GATE-3 counter declared)
    proxy/
      proxy.go            -- Registry of pre-built ReverseProxy per upstream, HandlerFor
    router/
      route.go            -- Route type, Group/Role constants, MuxPattern()
      player.go           -- BuildRoutes() (7 routes, the player table)
                             -- the admin table (was admin.go) moved to
                                services/gateway_dev/internal/router/dev.go
    server/
      server.go           -- dual HTTP listener (public + metrics), errgroup, shutdown
```

## Key types and functions GATE-2 will interact with

### `internal/config.Config`

There is no `Instance` field and no `GATEWAY_INSTANCE`; the split removed both,
along with `CORSAllowedOrigins` (which now belongs to `services/gateway_dev`, the
only browser-facing edge).

```go
type Config struct {
    ListenAddr     string
    MetricsAddr    string
    TLSCertFile    string
    TLSKeyFile     string
    ReadTimeout    time.Duration
    WriteTimeout   time.Duration
    IdleTimeout    time.Duration
    JWTClockSkew   time.Duration
    PlayerJWKSURL  string
    PlayerIssuer   string
    PlayerAudience string
    StaffJWKSURL   string
    StaffIssuer    string
    StaffAudience  string
    Upstreams      map[string]*url.URL   // GATEWAY_UPSTREAM_<NAME>_URL -> lowercase key
    RateLimitRPS   int
    RateLimitBurst int
}
```

`cfg.Upstreams` is a `map[string]*url.URL` parsed from env vars at startup.
Key normalisation: `GATEWAY_UPSTREAM_PHPADMIN_URL` becomes key `"phpadmin"`.
Config does NOT import internal/router -- it validates URL well-formedness only.

### `internal/apierr`

- `WithRequestID(ctx, id) context.Context` -- sets request ID on context
- `RequestIDFrom(ctx) string` -- reads request ID from context
- `WriteError(w, r, statusCode, code, message)` -- writes COM-5 JSON error

Both `obslog` and the future `internal/proxy` error handler should use `apierr.WriteError`.

### `internal/obslog`

- `Init()` -- sets default slog to JSON on stdout
- `RequestIDMiddleware(next)` -- outermost: reads/generates X-Request-Id, sets on context+response
- `LoggingMiddleware(next)` -- middle: logs method, route pattern, status, duration, group, request_id; observes `gateway_http_request_duration_seconds` histogram
- `RecoverMiddleware(next)` -- innermost (closest to mux): catches panics, writes COM-5 500; being innermost means Logging still writes its access log line after Recover returns normally
- `WithGroup(ctx, group) context.Context` -- stores route group name on context
  (to be called by GATE-2's route registration when a handler runs)
- `groupFrom(ctx) string` -- reads group from context (used by LoggingMiddleware)
- `statusWriter` -- captures status code, has `Unwrap()` for `http.NewResponseController`
  (GATE-2 needs this for `SetWriteDeadline` on stream routes)

Prometheus collectors (declared, registered in `init()`):
- `gateway_http_request_duration_seconds` histogram (route, method, status)
- `gateway_token_rejected_total` counter (reason, group) -- for GATE-3

### `internal/server`

- `Run(cfg *config.Config, ready *atomic.Bool, publicMux *http.ServeMux) error`
- `BuildPublicHandler(mux)` (exported) wraps: requestID -> logging -> recover -> mux
- `BuildMetricsMux(ready)` (exported) registers /healthz, /readyz, /metrics, pprof
- Public server uses cfg timeouts; metrics server has WriteTimeout=0 (pprof needs 30s+)
- Shutdown: 15s timeout, `errors.Join(pubErr, metErr)`

### `gateway.go` (entrypoint)

```go
func main() {
    obslog.Init()
    cfg, err := config.Load()
    // ...
    var ready atomic.Bool
    publicMux := http.NewServeMux()
    if err := server.Run(cfg, &ready, publicMux); err != nil {
        // ...
    }
}
```

The `publicMux` is currently empty. GATE-2's job is to register routes on it.

## What GATE-2 needs to build

Per techspec section 5 (GATE-2: Upstream routing to Essential Services):

### New packages

1. **`internal/router`** -- route types and route tables
   - `Group` enum: `GroupPublic`, `GroupPlayer`, `GroupStaff`
   - `Role` enum: `RoleViewer`, `RoleLiveOps`, `RoleAdmin`
   - `Route` struct: Method, Pattern, Upstream, StripPrefix, Group, MinRole, Stream
   - `player.go`: `BuildRoutes() []Route` (the player table; built as `BuildPlayerRoutes()`)
   - `admin.go`: `BuildAdminRoutes() []Route` — **superseded**: this table moved to
     `services/gateway_dev/internal/router/dev.go` in the split

2. **`internal/proxy`** -- reverse proxy registry
   - `Registry` struct: `proxies map[string]*httputil.ReverseProxy`
   - `NewRegistry(upstreams map[string]*url.URL) *Registry`
   - Per-proxy `http.Transport` with `MaxIdleConnsPerHost: 64`, `IdleConnTimeout: 90s`
   - `ErrorHandler` maps upstream errors to COM-5 502
   - `HandlerFor(route Route) http.HandlerFunc` -- wraps the shared proxy
   - Stream handling (`Stream: true`): `SetWriteDeadline(time.Time{})` to clear write
     timeout, shallow-copy proxy with `FlushInterval: -1` for immediate SSE flush

### Changes to existing code

- **`gateway.go`**: after `config.Load()`, build the route table (originally by
  switching on `cfg.Instance`, which the split removed — it is now just
  `router.BuildRoutes()`), create `proxy.NewRegistry(cfg.Upstreams)`, register each route
  on `publicMux` using `reg.HandlerFor(route)`. Set `obslog.WithGroup` on the
  handler context for each route's group.
- **Unmatched paths**: the default 404 on `publicMux` must return COM-5 JSON, not
  stdlib plain text. Register a catch-all or set a custom `NotFound` handler.

### Acceptance criteria (techspec)

1. A request to a registered route reaches the correct upstream with the correct
   path (prefix stripped where configured).
2. An unmatched path returns 404 in the COM-5 error shape.
3. A `Stream: true` route holds a connection open past `GATEWAY_WRITE_TIMEOUT`.
4. Killing an upstream mid-request surfaces as 502 in the COM-5 shape.

### Testing approach

- Use `httptest.NewTestServer` (Go 1.27.1 in-memory networking) for route tests
- Use `testing/synctest` for stream timeout tests
- Test that a more-specific route pattern (e.g. `/config/channels/live/releases`)
  takes precedence over the general `/config/` route -- techspec specifically calls
  this out as a required test
- Test upstream error handling: proxy to a closed listener -> 502 COM-5

## Known spec conflicts / open items

- COM-10 label conflict: `gateway_token_rejected_total` uses `reason`/`group` labels
  which are outside COM-10's allowed set (route/method/status/channel). Documented in
  `obslog/metrics.go` with a TODO. Reconcile before Dashboard metrics spec is finalised.
- AUTH-8 "next-hop hand-off payload" is unresolved (techspec §7). Route tables assume
  AUTH-8 has no effect on Gateway routing.
- ~~`/readyz` currently always returns 503~~ — **closed by GATE-3.** The JWKS
  registry sets it once every domain the route table uses has completed its first
  fetch, so on this service `/readyz` waits on Auth *and* PHP Admin Auth (the two
  staff manifest routes). See the note in `internal/config/config.go`.

## Dependencies GATE-2 will add

- No new go.mod dependencies expected. `net/http/httputil` is stdlib.
  If `StripPrefix` needs `strings.TrimPrefix`, that is also stdlib.
