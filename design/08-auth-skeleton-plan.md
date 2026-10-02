# Implementation plan — Auth service skeleton

**For:** the agent implementing this. Read this file end to end before touching code.
**Scope:** `services/auth/` only. Skeleton = COM-2 service template applied to Auth, plus the parts of `07-auth-techspec.md` that are foundation (schema, signing key, JWKS, token signer). No login/refresh business logic — see §9.
**Source of truth:** `design/00-common-stack.md` (§2.1, §3, §3.1), `design/06-auth-identity-contract.md` (§2–§6, §9.1, §9.2, §10), `design/07-auth-techspec.md` (§2, §3, §4). This plan resolves the gaps those docs leave; where they conflict with this plan, this plan wins for the skeleton and the conflict is listed in §10.

---

## 1. Constraints you must not break

These come from what is actually checked in, not from the design docs.

| Constraint | Why | Consequence |
|---|---|---|
| Entrypoint stays at `services/auth/auth.go`, `package main` | `ci/services/Jenkinsfile` runs `go build -o service .` inside `services/auth` and `docker build` with context `services/auth`. `05-gateway-techspec.md` §2 already made the same call for Gateway. | No `cmd/auth/` directory. Subcommands are dispatched from `auth.go`. |
| No imports from sibling modules | Jenkins does a **sparse checkout of `services/auth` only** — `services/go.work` is not present in CI. | Do not create or depend on a `platform` module (COM-13) in this task. `Claims` etc. live in `services/auth/internal/...`. |
| `go 1.27.1` + `toolchain go1.27.1` in `go.mod`; builder image `golang:1.27.1-bookworm` | `00-common-stack.md` §2.1 pins exact versions. | No floating tags anywhere. |
| Go and Docker are **not installed** on the authoring machine (Windows 11, checked 2026-09-18) | — | First step: `go version`. If missing, install Go 1.27.1 before anything else. If Docker is missing, DB-backed tests will skip — say so in your report; never claim they passed. |
| Repo is on Windows with `* text=auto` in `.gitattributes` | SQL/Dockerfile with CRLF break in containers (COM-1). | Add the explicit `eol=lf` lines from COM-1 for `*.sql`, `dockerfile`, `*.sh`, `*.yml` (root `.gitattributes`). Do not run `git add --renormalize .` repo-wide — only the auth files you create. |
| Don't touch other services | Scope. | No edits under `services/{gateway,session,...}`, `ci/`, or `services/go.work` (already lists `./auth`). |

---

## 2. Target layout

```
services/auth/
├── auth.go                      # package main: parse subcommand, call run{Serve,Migrate,Genkey}
├── go.mod                       # module github.com/otomo-live/otomo/services/auth
├── go.sum
├── dockerfile                   # rewritten per §7
├── .env.example                 # every AUTH_* var with its default or a placeholder
├── README.md                    # how to run: genkey → migrate → serve; env table
├── internal/
│   ├── config/
│   │   ├── config.go            # Load() (Config, error): env → struct, validate, fail fast
│   │   └── config_test.go
│   ├── server/
│   │   ├── server.go            # New(cfg, deps) → Run(ctx): two http.Servers, errgroup, shutdown
│   │   ├── middleware.go        # requestID, accessLog, metrics, recover
│   │   └── server_test.go
│   ├── api/
│   │   ├── errors.go            # WriteError(w, r, status, code, msg) — COM-5 shape
│   │   ├── jwks.go              # GET /.well-known/jwks.json
│   │   ├── stubs.go             # POST /auth/anonymous|refresh|logout → 501 not_implemented
│   │   ├── health.go            # /healthz, /readyz (readiness fn injected)
│   │   └── api_test.go
│   ├── token/
│   │   ├── claims.go            # Claims struct from 06 §9.1 (struct only, see §5.3)
│   │   ├── keyfile.go           # Generate/Write/Load Ed25519 private key (PKCS#8 PEM)
│   │   ├── jwks.go              # JWK/JWKSet types, Build([]PublicKey) → []byte, Cache (atomic.Pointer)
│   │   ├── signer.go            # Signer{kid, priv, iss, aud, ttl}.Issue(sub, now) (string, error)
│   │   └── token_test.go        # round-trip: Issue → parse via MicahParks/keyfunc against served JWKS
│   └── store/
│       ├── db.go                # NewPool(ctx, cfg) *pgxpool.Pool with Ping; Ready(ctx) error
│       ├── signing_keys.go      # InsertSigningKey, ListActiveSigningKeys, FindActiveByPublicKey
│       └── store_test.go        # gated on AUTH_TEST_DATABASE_URL (see §8)
└── migrations/
    ├── embed.go                 # //go:embed *.sql ; var FS embed.FS
    └── 00001_init.sql           # goose Up/Down for the four AUTH-1 tables
```

Five internal packages, flat, per `00-common-stack.md` §3.1 ("keep packages few and flat"). No `internal/domain` yet — nothing pure-logic exists in the skeleton to put there; add it when AUTH-4/5 land.

**Module path:** change `module auth/auth` → `module github.com/otomo-live/otomo/services/auth`. Nothing imports it yet, so this is free now and avoids `auth/auth/internal/...` import paths forever. The remote is `github.com/otomo-live/otomo` (see `ci/`).

---

## 3. Dependencies (add with `go get`, latest stable of each major)

| Module | Use |
|---|---|
| `github.com/jackc/pgx/v5` (+ `/pgxpool`, `/stdlib`) | Pool for the service; `stdlib` only to hand goose a `*sql.DB` |
| `github.com/pressly/goose/v3` | Embedded migrations, `migrate` subcommand |
| `github.com/golang-jwt/jwt/v5` | `SigningMethodEdDSA`, `RegisteredClaims` |
| `github.com/MicahParks/keyfunc/v3` | **test only** — proves Gateway's real consumer can parse our JWKS |
| `github.com/prometheus/client_golang` | `/metrics` |
| `golang.org/x/sync/errgroup` | Two listeners under one group |

Everything else is stdlib: `net/http`, `log/slog`, `crypto/ed25519`, `crypto/x509`, `encoding/pem`, `encoding/json`, `embed`, `net/http/pprof`, `sync/atomic`, `os/signal`, `uuid` (std in 1.27). No web framework, no ORM, no UUID library.

---

## 4. Configuration (`internal/config`)

All read once at startup. `Load()` returns an error listing **every** missing/invalid var, not just the first. `auth.go` logs it and exits 1.

| Var | Required | Default | Notes |
|---|---|---|---|
| `AUTH_LISTEN_ADDR` | no | `:8080` | Public listener (what Gateway proxies to). Jenkins maps host port → 8080. |
| `AUTH_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/` — never on the public listener |
| `AUTH_DATABASE_URL` | **yes** (`serve`, `migrate`, `genkey`) | — | `postgres://auth_rw:...@postgres:5432/auth` |
| `AUTH_DB_MAX_CONNS` | no | `8` | COM-14 budget; document it in README |
| `AUTH_SIGNING_KEY_PATH` | **yes** (`serve`) | — | PKCS#8 PEM, mounted read-only (COM-9) |
| `AUTH_ISSUER` | no | `https://auth.otomo.internal` | Must equal `GATEWAY_PLAYER_ISSUER` (06 §9.2) |
| `AUTH_AUDIENCE` | no | `otomo:player` | Must equal `GATEWAY_PLAYER_AUDIENCE` |
| `AUTH_ACCESS_TOKEN_TTL` | no | `15m` | 06 §6 |
| `AUTH_REFRESH_TOKEN_TTL` | no | `720h` | 06 §6 (30 d). Parsed and validated now; unused until AUTH-5 |
| `AUTH_PUBLIC_SESSION_URL` | no | `""` | AUTH-8; parsed as URL if non-empty, unused until AUTH-8 |
| `AUTH_READ_TIMEOUT` / `AUTH_WRITE_TIMEOUT` / `AUTH_IDLE_TIMEOUT` | no | `10s` / `30s` / `120s` | `ReadHeaderTimeout` is hardcoded 5s |
| `AUTH_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown` |
| `AUTH_LOG_LEVEL` | no | `info` | `slog.Level` |

`config_test.go`: (a) empty env for `serve` fails and the error names `AUTH_DATABASE_URL` and `AUTH_SIGNING_KEY_PATH`; (b) bad duration fails; (c) minimal valid env yields the defaults above.

**Note for Jenkins:** `ci/services/Jenkinsfile` runs `docker run` with no env. With fail-fast config the staging container will exit immediately on missing `AUTH_DATABASE_URL`. That is correct per spec (COM-9 is where env injection lands) — flag it in your report, do not add fake defaults to make it "start".

---

## 5. Subcommands and behavior (`auth.go`)

`auth` with no args = `serve`. `auth migrate`, `auth genkey`. Unknown → usage, exit 2. `-ldflags "-X main.version=..."` supplies `version`, logged at startup and exported as `auth_build_info{version}`.

### 5.1 `serve`

Startup order, each step fail-fast with a clear `slog.Error`:

1. `config.Load()`
2. `store.NewPool` + `Ping` (bounded ctx, ~5s). DB down at boot → exit 1; Compose/Jenkins restart handles retry. (Not the JWKS-style retry loop — that's Gateway's concern, per 06 §3; Auth has nothing to serve without its DB.)
3. `token.LoadPrivateKey(path)` → derive `ed25519.PublicKey`.
4. `store.FindActiveByPublicKey(pub)` → `kid`. Absent → exit 1 with "signing key at AUTH_SIGNING_KEY_PATH has no active row in signing_key; run `auth genkey`". **This is how the service learns its own `kid` — no `AUTH_SIGNING_KID` var, and a key/DB mismatch is caught at boot instead of as 401s at Gateway.**
5. `store.ListActiveSigningKeys` → `token.BuildJWKS` → pre-serialized `[]byte` into `token.JWKSCache` (`atomic.Pointer[[]byte]`, 07 §3). Rotation *trigger* is out of scope; the cache type exposes `Swap([]byte)` so it isn't.
6. Construct `token.Signer{kid, priv, iss, aud, ttl}` (07 §4 verbatim; `Header["kid"]` set; no `roles`).
7. `server.New(...)`, `Run(ctx)`: two `http.Server`s under `errgroup`; `signal.NotifyContext(SIGTERM, SIGINT)`; on signal, `Shutdown` both with `AUTH_SHUTDOWN_TIMEOUT`; exit 0 on clean shutdown, 1 if either listener errored.

Mark ready (`atomic.Bool`) only after step 6.

### 5.2 `migrate`

`goose.SetBaseFS(migrations.FS)`, `goose.SetDialect("postgres")`, `goose.Up(db, ".")` using a `*sql.DB` from `pgx/v5/stdlib`. Exit 0/1. Idempotent (re-run is a no-op). This is what COM-8's "run migrations" step invokes as a one-shot container.

### 5.3 `genkey`

Flags: `-kid` (default `auth-YYYY-MM-DD` from current UTC date), `-out` (default `$AUTH_SIGNING_KEY_PATH`, required if unset). Behavior:

1. Refuse if `-out` exists (never overwrite — an overwritten key invalidates every outstanding token; the operator must delete deliberately).
2. `ed25519.GenerateKey(rand.Reader)`.
3. Write private key as PKCS#8 PEM (`x509.MarshalPKCS8PrivateKey`), file mode `0600`.
4. `INSERT INTO signing_key (kid, public_key, active) VALUES ($1, $2, true)` — `public_key` is the raw 32-byte `ed25519.PublicKey`. Duplicate `kid` → exit 1 and **delete the file you just wrote** so the operator can retry cleanly.
5. Print `kid` and path to stdout.

Rotation = run `genkey` again with a new `kid`, swap the mounted file, restart; old row is flipped `active=false` by hand ≥15 min later (06 §6). Do not build a rotation command now.

### 5.4 `token` package details

- `Claims` = exactly 06 §9.1's struct (`jwt.RegisteredClaims` + `Roles []string json:"roles,omitempty"`). **Omit `HasRoleAtLeast`** — it depends on Gateway's `router.Role` and is verification-side only; Auth never sets `Roles`.
- `Signer.Issue(sub string, now time.Time)` builds claims per 07 §4 (`Audience: jwt.ClaimStrings{aud}` — array form, 06 §4), sets `exp/nbf/iat`, `Header["kid"]`, signs with `SigningMethodEdDSA`.
- `BuildJWKS(keys []SigningKey) ([]byte, error)`: each entry `{"kty":"OKP","crv":"Ed25519","kid":...,"x":base64url(pub),"use":"sig","alg":"EdDSA"}` (06 §3). Use explicit struct tags — the `jwk` struct in 07 §3 with `json:"..."` is a placeholder, fill in the real tag names. Keys sorted by `kid` for deterministic output.

---

## 6. HTTP surface

### Public listener (`AUTH_LISTEN_ADDR`)

| Route | Handler | Status |
|---|---|---|
| `GET /.well-known/jwks.json` | write cached bytes, `Content-Type: application/json`, `Cache-Control: no-store` | real |
| `POST /auth/anonymous` | `WriteError(501, "not_implemented", ...)` | stub (AUTH-4) |
| `POST /auth/refresh` | same | stub (AUTH-5) |
| `POST /auth/logout` | same | stub (AUTH-5) |
| anything else | `404 not_found` in COM-5 shape (custom `ServeMux` fallback) | — |

Paths are the illustrative ones from 06 §8; Gateway proxies `/auth/` as a prefix so they can change later without a Gateway edit. Register with `ServeMux` method patterns (`"POST /auth/anonymous"`); 405 responses also go through `WriteError`.

### Internal listener (`AUTH_METRICS_ADDR`)

| Route | Behavior |
|---|---|
| `GET /healthz` | always `200 ok` once the server is up |
| `GET /readyz` | `200` iff the ready flag is set **and** `pool.Ping` succeeded within the last 10s (cache the last ping result — don't hit Postgres on every probe); else `503` with COM-5 body |
| `GET /metrics` | `promhttp.Handler()` |
| `/debug/pprof/*` | `net/http/pprof` handlers, registered on this mux only |

### Middleware chain (public listener, outermost first)

1. **recover** → `500 internal_error` via `WriteError`, log with stack.
2. **request ID** (COM-3): read `X-Request-Id`; if absent generate `uuid.New()`-style; store in ctx; set on response header. Auth is behind Gateway so the header is normally present.
3. **access log**: one `slog` line per request — `method`, `route` (the mux **pattern**, not raw path — COM-10), `status`, `duration_ms`, `request_id`. Use `http.Request.Pattern` (Go ≥1.23) after the mux has matched; wrap the mux, not each handler.
4. **metrics**: `auth_http_requests_total{route,method,status}`, `auth_http_request_duration_seconds{route,method}` histogram. Labels only from COM-10's allowed set. Plus `auth_jwks_keys_active` gauge and `auth_build_info`.

### Error body (COM-5)

```json
{ "error": { "code": "not_implemented", "message": "...", "request_id": "..." } }
```

`WriteError` is the **only** way any handler writes a 4xx/5xx. `request_id` comes from ctx.

---

## 7. Dockerfile (replaces current `services/auth/dockerfile`)

```dockerfile
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG GIT_SHA=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${GIT_SHA}" -o /out/auth .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/auth /auth
EXPOSE 8080 9090
ENTRYPOINT ["/auth"]
CMD ["serve"]
```

Keep the lowercase `dockerfile` filename — Jenkins and the other services already use it. Runs as uid 65532; document that the mounted key file must be readable by that uid (`0640` group-readable or owned by 65532). `docker run … /auth migrate` and `… /auth genkey` work because `ENTRYPOINT` is the binary.

---

## 8. Tests

Run with `go test -race ./...` (Jenkins currently runs without `-race`; add `-race` to your local runs regardless).

| Package | Test | Needs |
|---|---|---|
| `config` | missing/invalid/defaults (see §4) | none |
| `api` | `WriteError` shape and `request_id` propagation; JWKS handler returns cached bytes with correct headers; stubs return 501 in COM-5 shape; unknown route 404 in COM-5 shape | `httptest` |
| `server` | request ID generated when absent, echoed when present; access-log line contains `route` pattern not raw path; `/metrics` and `/debug/pprof/` are **404 on the public listener** | `httptest` |
| `token` | PEM write → load round-trip; `BuildJWKS` output parses with `keyfunc.NewJWKSetJSON` (or `jwkset`), `kty/crv/alg/use` per 06 §3; `Signer.Issue` → `jwt.ParseWithClaims` using `keyfunc` against an `httptest.Server` serving our JWKS, with `WithValidMethods(["EdDSA"])`, `WithIssuer`, `WithAudience`, `WithExpirationRequired` — i.e. **Gateway's exact parser options from `05-gateway-techspec.md` §6.3**; `Roles` is absent from the emitted JSON | `keyfunc` |
| `store` | `goose.Up` on empty DB, then `Up` again is a no-op; `InsertSigningKey` + `FindActiveByPublicKey` round-trip; duplicate `kid` errors | Postgres |

**Postgres for tests:** read `AUTH_TEST_DATABASE_URL`; if unset, `t.Skip("AUTH_TEST_DATABASE_URL not set")`. Do not use testcontainers in this task — Docker isn't on the authoring machine and its availability on the Jenkins agent is unknown, and a hard dependency would turn CI red for environmental reasons. Document in README how to run them against any throwaway Postgres.

---

## 9. Explicitly out of scope (do not implement)

- AUTH-4 device login logic, AUTH-5 refresh rotation / family revocation, AUTH-8 `services` payload. Handlers are 501 stubs; the schema for them **is** created (§2 migrations) because AUTH-1 is foundation.
- Key rotation automation, JWKS rebuild on `LISTEN/NOTIFY`, multi-key signing.
- The shared `platform` module (COM-13), Compose file (COM-7), Postgres provisioning (COM-6), Jenkinsfile changes (COM-8/12), secrets injection (COM-9).
- Gateway, admin-auth, any other service.
- Rate limiting (Gateway does it).

---

## 10. Decisions this plan makes that the docs left open

Record these in the README so they don't get re-litigated:

1. **`kid` discovery by public-key lookup**, not an env var (§5.1 step 4). 07 §3 says "public half + kid inserted into `signing_key`" but never says how `serve` knows which `kid` its file corresponds to.
2. **Private key file format: PKCS#8 PEM.** 07 §3 says only "written to a file".
3. **`genkey` never overwrites and rolls back the file on DB insert failure** (§5.3).
4. **DB down at boot = exit 1**, no retry loop (§5.1 step 2). Contrast with Gateway's JWKS retry, which is required by 06 §3 because Gateway can usefully serve public routes without it; Auth cannot serve anything without Postgres.
5. **Module path** renamed to the repo path (§2).
6. **Store tests gated on `AUTH_TEST_DATABASE_URL`**, not testcontainers (§8).
7. **`iss`/`aud` defaults** are the 06 §9.2 proposed values — still "needs confirmation" per 06 §7. They're env vars, so confirming later is a config change, not a code change.

---

## 11. Acceptance checklist (report against this, item by item)

- [ ] `go build ./...` and `go vet ./...` clean inside `services/auth` **without** `go.work` present (simulate CI: run from a temp copy of just that directory, or `GOWORK=off`).
- [ ] `go test -race ./...` passes; store tests skip cleanly with no `AUTH_TEST_DATABASE_URL`. If you could run them against a Postgres, say which.
- [ ] `auth` with empty env exits 1 and the error names every missing var.
- [ ] `auth genkey -out k.pem` twice: second run refuses to overwrite.
- [ ] `auth migrate` twice: second run is a no-op.
- [ ] `serve` with a key file that has no matching `signing_key` row exits 1 with the message in §5.1 step 4.
- [ ] `/readyz` is 503 until startup completes, 200 after; `/healthz` 200 throughout.
- [ ] `/metrics` and `/debug/pprof/` return 404 on `AUTH_LISTEN_ADDR`.
- [ ] `GET /.well-known/jwks.json` output is accepted by `MicahParks/keyfunc/v3` and a `Signer.Issue` token verifies with Gateway's exact parser options (test in §8).
- [ ] `SIGTERM` during an in-flight request lets it finish (test with `synctest` or a slow handler + `httptest`), then exits 0.
- [ ] Every 4xx/5xx from the public listener is the COM-5 body with a `request_id`.
- [ ] `docker build services/auth` succeeds (if Docker available) and the image runs as non-root; if Docker is unavailable, say so.
- [ ] `.gitattributes` gained the COM-1 `eol=lf` lines; new `.sql`/`dockerfile` files are LF.
- [ ] Nothing outside `services/auth/`, root `.gitattributes`, and this doc's README was modified; nothing committed.
