# Gateway Testing Guide (player gateway)

This service is the **player** gate. The admin and dev edge is `services/gateway_dev`,
and its own testing guide lives there.

## Quick start

You need Go 1.27.1 on your `PATH`. On Windows, use Git Bash for the smoke test.

1. **Run the automated tests** (about 10 seconds). From `services/gateway`:

   ```bash
   go test ./...
   ```

   Every package should print `ok`.

2. **Run the smoke test** (about 30 seconds). From the repo root:

   ```bash
   bash services/gateway/testdata/smoke/smoke.sh
   ```

   It builds the gateway, starts fake key and upstream servers, sends real
   requests with real signed tokens, and stops everything at the end. Each line
   shows what came back and, in `[expect ...]`, what should have come back. If
   they match on every line, the gateway works.

3. **Poke it by hand** (optional): see section 2.

You do not need Docker, a database, Auth or PHP Admin Auth for any of this.

## 1. Automated tests

From `services/gateway`:

```powershell
go test -v ./...
go test -race ./...
go vet ./...
gofmt -l .
govulncheck ./...
```

These are the same checks GitHub Actions runs on every push and pull request
that touches `services/gateway/**`. `-race` needs a C compiler (gcc) on `PATH`
on Windows.

CI also runs a **shared-package drift** check: `services/gateway_dev` keeps
copies of this service's `internal/apierr/apierr.go`,
`internal/authn/{claims,jwks,middleware,fixture_test}.go`,
`internal/router/route.go`, `internal/proxy/proxy.go` and
`internal/server/server.go`, and the build fails if the two copies differ. If
you change one of those files, make the same change in both services.

On Windows, `gofmt -l .` may list files that only have CRLF line endings. Git
stores them as LF, which is what CI checks, so this is not a real failure.

Race detector clean, vet clean, no unformatted files. No pass count is written here on
purpose: it goes stale on the next test added, and a stale count in a testing guide is
worse than no count.

### Test coverage

`gateway_test.go`, `verify_upstreams_test.go` and `internal/config/config_test.go`:

| Test | What it verifies |
|------|-----------------|
| `TestWriteError_COM5Shape` | COM-5 error body structure (`code`, `message`, `request_id`) |
| `TestHealthz_Returns200` | `/healthz` on the metrics port returns 200 |
| `TestReadyz_Returns503UntilReady` | `/readyz` returns 503 until the JWKS registry reports ready |
| `TestPublicListener_InternalEndpointsUnreachable` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/` return 404 on the public port |
| `TestRequestIDMiddleware_GeneratesID` | X-Request-Id generated and set on context + response |
| `TestRequestIDMiddleware_PreservesInbound` | Inbound X-Request-Id preserved (not overwritten) |
| `TestRecoverMiddleware_CatchesPanic` | Panic returns COM-5 500 with `request_id`; access log emitted with status 500 and the same ID |
| `TestGracefulShutdown_InFlightRequestCompletes` | In-flight request completes during the shutdown window |
| `TestStripPrefix_ForwardsWithTrimmedPath` | A route with `StripPrefix` forwards the trimmed path |
| `TestUnmatchedPath_ReturnsCOM5_404` | Unmatched path returns COM-5 JSON 404, not plain text |
| `TestUpstreamDown_ReturnsCOM5_502` | Dead upstream returns COM-5 JSON 502, not a connection reset |
| `TestStreamRouteHoldsConnection` | `Stream: true` route holds the connection past `WriteTimeout` |
| `TestVerifyUpstreams_MissingUpstreamFails` | Missing upstream key in the registry fails startup |
| `TestVerifyUpstreams_AllPresentSucceeds` | All upstreams present passes verification |

`internal/authn/middleware_test.go` carries the GATE-3 suite (the two identity domains,
401 vs 403, signature/issuer/audience ordering) plus the techspec §9 route-table guards.
Two of those guards are worth knowing about, because they are the ones that would have to
change if this service were ever widened:

- `TestNoPublicRouteUnderAPI`: every route under `/api/` must name a non-public group.
- `TestUsedGroupsMatchesTheTable`: the player table uses **both** domains, which is true
  because of the two staff manifest routes.

`route_policy_test.go` (GATE-5) builds the gateway exactly as `main` does and
checks the route table against the techspec §9 policy:

| Test | What it verifies |
|------|-----------------|
| `TestRoutePolicy_MatchesSpec9` | Every route matches exactly one §9 prefix and has that prefix's group |
| `TestRoutePolicy_FieldsConsistentWithGroup` | No public or player route has a `MinRole` (it would never be checked) |
| `TestRoutePolicy_FailClosedMatrix` | Every route with no token, a player token and a staff token: served, or COM-5 401 and never proxied |
| `TestRoutePolicy_CrossDomainPrefixesNeverReachable` | A player token never reaches staff paths, a staff token never reaches player paths |
| `TestRoutePolicy_AdminPathsNotServed` | Every admin path is a COM-5 404, even with a staff admin token |
| `TestRoutePolicy_JWKSEndpointsNeverProxied` | `/.well-known/*` is a COM-5 404 and never proxied |

`ratelimit_test.go` and `internal/ratelimit/ratelimit_test.go` (GATE-3 rate limiting,
techspec §6.4):

| Test | What it verifies |
|------|-----------------|
| `TestLoginPatterns_ExistInRouteTable` | Every login-limited pattern is a real route |
| `TestRateLimit_LoginRouteUsesStricterLimiter` | `/auth/*` gets its own smaller bucket; a 429 is COM-5 and not proxied; other routes are unaffected |
| `TestRateLimit_GeneralLimiterAppliesBeforeAuth` | Over the limit is 429 before any token check, whatever token is sent |
| `TestRateLimit_429IsNotATokenRejection` | A 429 has an empty `reason` in the access log and does not move `gateway_token_rejected_total` |
| `TestLimiter_PerIPBuckets` | Each client IP has its own bucket; a new port does not reset it |
| `TestLimiter_429IsCOM5` | A 429 body is `rate_limit_exceeded` in the COM-5 shape |
| `TestSweeperEvictsIdleEntries` | Idle per-IP entries are removed, so memory cannot grow without bound |

`TestServeMuxSpecificity_SpecificRouteWins` (the `RoleAdmin`-over-`RoleLiveOps` override on
`POST /api/admin/config/channels/live/releases`) moved to `services/gateway_dev` with the
route it guards. The admin table no longer exists in this binary.

## 2. Manual testing (live demo)

### Prerequisites

- Go 1.27.1
- Branch checked out

### Step 1: Start the fake upstreams

Open a terminal in `services/gateway/testdata/manual`:

```powershell
go run fake_upstream.go
```

This starts 7 stub servers (ports 9001-9007) that echo every request as JSON.

### Step 2: Start the gateway

Open a second terminal in `services/gateway/testdata/manual`:

```powershell
.\run-player.ps1
```

### Step 3: Test the endpoints

Open a third terminal. There is no admin runner here; `run-admin.ps1` moved to
`services/gateway_dev`, and against this service those paths must 404.

**Public routes: should return JSON with the upstream name and request_id:**

```powershell
curl http://127.0.0.1:8080/auth/login
curl http://127.0.0.1:8080/patch/v1/live/manifest
curl "http://127.0.0.1:8080/patch/v1/blob/abc123"
```

**Player routes: without a token these must be 401 `missing_token`:**

```powershell
curl http://127.0.0.1:8080/api/player/session/info
```

Expected:

```json
{"error":{"code":"missing_token","message":"...","request_id":"..."}}
```

**Staff routes: also 401 without a token.** `GET /patch/v1/dev/manifest` and
`GET /patch/v1/staging/manifest` take a **staff** token from PHP Admin Auth
(`design/03-patch-minimal.md` PAT-B6), so this service loads a second issuer:

```powershell
curl http://127.0.0.1:8080/patch/v1/dev/manifest
```

A **player** token here must 401 `invalid_signature`, not 200, and not
`iss_mismatch`: the player key is not in the staff registry, so the signature check is
what fails first. That is the intended order (GATE-3).

**The admin table is not in this binary.** All of these must 404:

```powershell
curl http://127.0.0.1:8080/api/admin/config/settings
curl http://127.0.0.1:8080/admin/dashboard
curl http://127.0.0.1:8080/admin-auth/login
```

**Unmatched path: should return COM-5 404:**

```powershell
curl http://127.0.0.1:8080/no/such/path
```

Expected:

```json
{"error":{"code":"not_found","message":"the requested path does not exist","request_id":"..."}}
```

**Login rate limit: 15 fast requests to `/auth/*` should start returning 429.**
In Git Bash (one curl process, so the requests really are back to back):

```bash
curl -s -w '%{http_code} ' -X POST \
  $(for i in $(seq 1 15); do printf -- '-o /dev/null http://127.0.0.1:8080/auth/login '; done)
```

Expected: about ten `200`s, then `429`s. A 429 body is
`{"error":{"code":"rate_limit_exceeded",...}}`. Wait a few seconds and it
recovers (5 requests per second refill).

**Internal endpoints on the public port: should all return 404:**

```powershell
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/metrics
curl http://127.0.0.1:8080/debug/pprof/
```

**Internal endpoints on the metrics port: should work:**

```powershell
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:8081/readyz
curl http://127.0.0.1:8081/metrics
```

`/readyz` stays 503 until **both** JWKS documents have been fetched (see the note in
`internal/config/config.go`).

### What to look for

1. **Routing correctness:** each permitted request reaches the right upstream (check the
   `"upstream"` field); every admin path 404s.
2. **Request ID propagation:** every response has an `X-Request-Id` header; the upstream
   receives the same ID.
3. **Access logs:** gateway stdout shows one JSON log line per request with `method`,
   `route`, `status`, `duration`, `request_id`.
4. **Port isolation:** `/healthz`, `/metrics`, `/debug/pprof/` return 404 on the public
   port but work on the metrics port.
5. **COM-5 error shape:** unmatched paths return `{"error":{"code":"not_found",...}}`,
   not plain text.
6. **Prometheus metrics:** `curl http://127.0.0.1:8081/metrics` shows
   `gateway_http_request_duration_seconds` with route/method/status labels. The metric
   names are shared with `gateway_dev` deliberately; see the package comment in
   `internal/obslog/metrics.go` before renaming anything.

## 3. Smoke test (no Docker)

```bash
bash services/gateway/testdata/smoke/smoke.sh
```

`testdata/smoke/smoke.sh` is the whole edge in one run: it builds this service from
the working tree, generates its own keys and mints its own tokens, starts a JWKS
endpoint, starts the echo upstreams and the gateway, prints one line per check with
the observed status and COM-5 code, and stops everything again. It needs Go on
`PATH` and nothing else: no Docker, no database, no Auth, no PHP Admin Auth. The
ports are its own (public `18080`, metrics `18081`, JWKS `18082`) so it does not
collide with the manual runner in §2; the echo upstreams are the same
`testdata/manual/fake_upstream.go`, on 9001-9007.

What it holds, in order:

1. no token on `/api/player/session/me` → `401 missing_token`
2. a player token there → `200` from the session upstream
3. a staff token there → `401 invalid_signature`
4. a player token on `/patch/v1/dev/manifest` → `401 invalid_signature`, **not**
   `iss_mismatch`: the kid is what fails first
5. a staff token there → `200` from the patch upstream
6. every admin path 404s
7. `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`: 404 on the public port, 200 on
   the metrics port
8. an unmatched path → the COM-5 404 shape
9. 15 back-to-back `POST /auth/login` → ten `200`s, then `429 rate_limit_exceeded`;
   a public patch route straight after is still `200` (separate bucket)
10. that no request id, player subject or staff subject appears in any `/metrics`
   label, with the label names printed so they can be read
11. that the series are `gateway_*` and no `gateway_dev_*` exists
12. one COM-5 body and one structured access log line, for eyeballing

`testdata/smoke/keyserver/` is the only helper. It generates both Ed25519 keypairs at
startup, serves the two JWKS documents, and writes the five tokens the script
presents. It imports this service's own `internal/authn.Claims`, so a token it mints
is the exact production payload shape rather than a lookalike. Everything lives
under `testdata/`, so `go build ./...` and `go vet ./...` neither see it nor need it.
