# Milestone 1 — Common Stack & Conventions

Shared by the Dashboard, Config, Patch and Session documents. Read this first; the other documents reference task IDs here (`COM-*`, `WEB-*`).

---

## 1. Service map for Milestone 1

| Service | Identity domain | Purpose in M1 |
|---|---|---|
| Auth | Player | Player login, JWKS (already planned) |
| admin-auth | Staff | Staff login, staff JWKS (already planned) |
| Gateway | Player, plus staff for two Patch routes | Routing, TLS, token validation, rate limiting — player edge |
| Gateway (dev) | Staff | The same, for the admin and dev edge |
| Config | Staff | Author, validate, version and publish config/content |
| Patch (minimal) | Player / public | Serve published manifests and content blobs |
| Session (minimal) | Player | Profiles, presence, friends, parties |
| Dashboard | Staff | Metrics, logs, audit trail |
| Admin WebUI (Ionic) | Staff | One frontend app hosting Config + Dashboard modules |

**One admin app, not two.** Config and Dashboard are modules inside a single Ionic app. They share login, navigation, the HTTP client and the build pipeline. Building two separate apps doubles auth and CI work for no gain.

### Two Gateway services, one administrative boundary

Gateway is **two services**: `services/gateway` serves players, and `services/gateway_dev` serves admins and devs. They are separate binaries with separate images, separate config surfaces and separate CI jobs. This is still a load-isolation and blast-radius decision — player traffic (manifest polling, blob downloads, session heartbeats/long-polls) is bursty and high-volume, and it must not be able to starve the admin tooling staff need most during an incident — but it is now also an identity decision, which is why it is a code fork and not a flag.

**This section previously specified one image deployed twice, distinguished by `GATEWAY_INSTANCE=player|admin`.** That design is superseded. The reason is not the load argument, which a flag satisfies, but two things a flag cannot express:

1. **The two audiences are authenticated by two different identity providers.** Players carry a token from Auth (`iss https://auth.otomo.internal`, `aud otomo:player`, no `roles` claim); staff carry one from admin-auth (`iss https://admin-auth.otomo.internal`, `aud otomo:staff`, a `roles` array). See `06-auth-identity-contract.md`. A flag that selects between them means the distinction lives in an environment variable on a container that faces the public internet.
2. **Whether admin routes are reachable must not be a runtime decision.** With one image, the player-facing deployment *contains* the admin route table and is one env var away from serving it. With two services, `gateway` has no admin route table to load — a mistaken or hostile env var has nothing to select. That property is worth the duplicated shared packages, and it is the one that `05-gateway-techspec.md` §9's route-table tests can actually assert.

| | `gateway` | `gateway_dev` |
|---|---|---|
| Repo | `services/gateway/` | `services/gateway_dev/` |
| Exposure | Public port | Ideally not public at all — bind to the private network, VPN, or IP-allowlist depending on what the school VM's networking allows |
| Routes it carries | Player + public, plus the two staff Patch manifests | Staff only |
| Identity domains it loads | Auth **and** admin-auth (see below) | admin-auth only |
| Env prefix | `GATEWAY_*` | `GATEWAY_DEV_*`, except the three staff-domain names, which are shared verbatim |
| Rate limiting | Tuned for high volume, per-IP | Tuned for low volume, stricter on login |

**Route pinning is per-route.** COM-4's rule — each route accepts exactly one issuer — is a property of the `Route` entry, not of which service is running. The split decides which physical listener carries the traffic; it does not change who is allowed to call what. The one route that crosses the split is Patch's `dev`/`staging` manifest, which lives on `gateway` (it's Patch traffic, and player-volume infrastructure should serve it) but still requires a **staff** token, so `gateway` loads the staff JWKS source too, just for that route. Don't assume a service only ever validates one issuer — assume a route does. The concrete cost of that decision: `gateway`'s `/readyz` waits on admin-auth's JWKS, so a staff-side outage holds the player gateway at 503 even though every player route could serve. That is an accepted trade for keeping one dev-channel client pointed at one base URL.

#### `gateway` route table

| Route prefix | Upstream | Accepted issuer |
|---|---|---|
| `/auth/*` | Auth | none (login endpoints) |
| `/patch/v1/live/*` | Patch | none (public) |
| `/patch/v1/{dev,staging}/*` | Patch | Staff |
| `/api/player/session/*` | Session | Player |

#### `gateway_dev` route table

| Route prefix | Upstream | Accepted issuer | Minimum role |
|---|---|---|---|
| `/admin-auth/*` | admin-auth | none (login endpoints) | — |
| `/admin/*` | Admin WebUI static files (nginx) | none (static assets; data calls behind them are protected) | — |
| `/api/admin/config/*` | Config | Staff | `viewer` |
| `POST /api/admin/config/channels/live/releases` | Config | Staff | `admin` |
| `POST /api/admin/config/packs` | Config (upload) | Staff | `live_ops` |
| `/api/admin/users/*` | admin-auth | Staff | `admin` |
| `/api/admin/dashboard/*` | Dashboard | Staff | `viewer` |
| `/api/admin/dashboard/logs/tail` | Dashboard (streaming) | Staff | `viewer` |
| `/api/admin/session/*` | Session (admin endpoints) | Staff | `viewer` |

Roles are ordinal, `viewer(1) < live_ops(2) < admin(3)`, and a minimum role means "that role or higher". The release-publish row overrides the `/api/admin/config/*` prefix above it: `ServeMux` resolves the more specific pattern first, so publishing a live release takes an `admin` even though reading config takes a `viewer`.

A validly signed token from the wrong issuer, or missing where one is required, returns `401` regardless of which service receives the request; a correctly issued but under-privileged staff token returns `403`. See `05-gateway-techspec.md` for the concrete route-table type and implementation, and `services/gateway_dev/README.md` for the reasoning behind the split.

---

## 1a. Network topology and trust boundaries

**North-south vs. east-west.** Gateway exists to sit at the edge where untrusted callers — a player's Godot client, a staff member's browser, both outside your control — meet the system (*north-south* traffic). Calls between your own services — Dashboard reading Config's audit log, Patch reading the `config` Postgres database — are *east-west*: both ends are your own code, so they don't need to route through Gateway. This distinction is about trust boundary, not physical host. Two of your own VMs on a private network are still inside the trust boundary even though bytes cross a real NIC instead of a container bridge; a public internet path between them would not be.

**M1 (now):** one VM, one Docker Compose bridge network. Every service resolves every other by Compose's built-in DNS (`http://config:8080`), configured via env var (`CONFIG_INTERNAL_URL`, etc.), never hardcoded. Only the two Gateway services publish ports.

**Later (deferred, see NET-1):** if services split across multiple VMs, only the Gateway VMs get a public-facing NIC. Every backend VM (services, Postgres, Valkey) has no listener reachable from the public internet, enforced by the host firewall — not assumed from "nobody happens to hit that port." All VMs join a private overlay network and address each other by the same hostnames as today; only what the hostname resolves to changes. Defense in depth still applies here: a private network keeps outsiders out, but every service still re-verifies the token's issuer/audience itself (COM-4) rather than trusting "this came from inside the network" as authorization.

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| NET-1 | Docker Swarm overlay network: `docker swarm init` on the first VM, `docker swarm join` on additional VMs, `docker network create -d overlay --attachable --opt encrypted otomo-net`; migrate the Compose stack to a Swarm stack file (`docker stack deploy`) or keep Compose for single-VM services and only move the ones that need to split. **Not required for M1** — the single-VM Compose network covers it. Pick this up when a second VM is actually provisioned; can be layered on retroactively without touching service code, since services only ever address each other by hostname | A service on VM B reaches a service on VM A by the same hostname it used within one VM; no service has a hardcoded IP | COM-7 |

---

## 2. Tools you will need

### 2.1 Backend language: Go

All M1 backend services (Config, Patch, Session, Dashboard, Gateway, admin-auth) are written in **Go 1.27.1**. Staff authentication was originally planned in PHP; it is now the Go service `services/admin_auth`, so there is no PHP runtime left in the stack. Pin the exact version in every `go.mod` (`go 1.27.1` and `toolchain go1.27.1` directives) and in the Dockerfile builder image tag (`golang:1.27.1-...`), not a floating `1.27` or `latest` — patch releases can change vet/lint behavior (`go test` now runs the `stdversion` check by default in 1.27) and you want every service and every dev machine building against the same toolchain. `go1.27.1` (2026-09-01) is a patch release — fixes to cgo, the compiler, runtime, `go fix`, and `database/sql`, `debug/elf`, `encoding/json`, `net/http`, `os`, `simd`/`archsimd` — with no API changes affecting anything below.

Go 1.27 features this plan relies on:
- **`encoding/json/v2` and `encoding/json/jsontext`** are now standard packages, no experiment flag. The classic `encoding/json` also runs on the v2 engine, with stricter defaults: invalid UTF-8 and duplicate object keys are rejected. That strictness is what you want for config validation.
- **Standard `uuid` package** for generating and parsing UUIDs, so no third-party UUID dependency is needed.
- **`goroutineleak` profile** in `runtime/pprof` is generally available. It is useful for catching stuck long-poll and SSE goroutines.

#### Libraries

Keep dependencies few. Prefer the standard library, and add a library only where writing it yourself is a project of its own.

| Library | What it is | Role | Notes |
|---|---|---|---|
| `net/http` (std) | HTTP server and client; `ServeMux` supports method + path-parameter patterns (`GET /players/{id}`) | All HTTP | No web framework needed |
| `log/slog` (std) | Structured logger | JSON logs to stdout | Use `slog.NewJSONHandler(os.Stdout, …)` |
| `context` (std) | Cancellation and deadlines | Every DB/Valkey/HTTP call takes the request's context | Client disconnect cancels long-polls and SSE automatically |
| `crypto/sha256`, `io` (std) | Hashing, streaming | Content addressing; hash while streaming with `io.MultiWriter` | Never buffer whole uploads |
| `embed` (std) | Compile files into the binary | Embed SQL migrations and query templates | One self-contained binary |
| `net/http/pprof` (std) | Runtime profiler endpoints | CPU, heap, goroutine, goroutine-leak profiles | Serve on a **separate internal port**, never through Gateway |
| **`jackc/pgx/v5`** | PostgreSQL driver + `pgxpool` connection pool | All Postgres access, including `LISTEN/NOTIFY` | Use pgx directly, not via `database/sql`, and no ORM |
| **`sqlc`** (optional, build-time) | Generates typed Go functions from hand-written SQL | Removes row-scanning boilerplate without reflection at runtime | Skip if you prefer writing `pgx` scans by hand |
| **`pressly/goose`** | SQL migration tool usable as a library or CLI | Migrations embedded in each service; run via a `migrate` subcommand | Alternative: `golang-migrate/migrate` |
| **`prometheus/client_golang`** | Prometheus instrumentation | `/metrics`, counters, histograms, gauges | |
| **`santhosh-tekuri/jsonschema`** | JSON Schema validator (draft 2020-12) | Config validation | Check the current major version before adding |
| **`valkey-io/valkey-go`** | Valkey client with pipelining and pub/sub | Session | `redis/go-redis` also works against Valkey |
| **`MicahParks/keyfunc`** + **`golang-jwt/jwt/v5`** | JWKS fetching/caching + JWT verification | COM-4 middleware | Or verify with `crypto/rsa`/`crypto/ecdsa` + `jsontext` yourself; the library route is less error-prone |

#### Go tooling

| Tool | What it is | Used for |
|---|---|---|
| `go test` (+ `-race`) | Test runner and data-race detector | Unit/integration tests; always run with `-race` in CI |
| `testing/synctest` (std) + `httptest` | Deterministic time/concurrency tests and in-memory HTTP servers | Testing timeouts, long-poll and TTL logic without real sleeps |
| **`testcontainers-go`** | Starts Postgres/Valkey containers from Go tests | Integration tests against real databases |
| **`golangci-lint`** | Aggregated linter runner | CI quality gate |
| **`govulncheck`** | Go's official vulnerability scanner | CI: fails on reachable known vulnerabilities |
| `go tool pprof` | Profile viewer | Measuring before optimizing |

#### Performance conventions (matching your data-oriented approach)

- No ORM, no reflection-heavy frameworks. SQL is written by hand (optionally compiled by `sqlc`).
- Allocate result slices with known capacity; stream large bodies with `io.Copy` rather than reading into memory.
- Hot read paths (Patch manifest, Dashboard overview cache) hold pre-serialized `[]byte` responses and swap them atomically with `atomic.Pointer`, so a request does no JSON encoding at all.
- Benchmark with `testing.B` and profile with pprof before changing code for speed.

### 2.2 Infrastructure

| Tool | What it is | Used for |
|---|---|---|
| **Docker** + **Docker Compose** | Container runtime and multi-container orchestration file | Running every service on the VM |
| **Docker Registry** (`registry` image, CNCF Distribution) | Private image store | Jenkins pushes versioned images; deploy pulls them; enables rollback |
| **PostgreSQL** | Relational database | One instance, separate database + user per service |
| **Valkey** | In-memory key/value store (BSD-licensed Redis fork) | Session presence and event delivery; Matchmaker queues next milestone |
| **nginx** | HTTP server | Serves the Ionic static build and Patch content blobs |
| **Jenkins** | CI/CD server | Build, test, image, deploy. Never serves runtime traffic |
| **goose** (embedded in each Go service) | Versioned SQL schema changes | Run as a one-shot container (`service migrate`) before each service deploy |

Pin exact image tags (never `latest`) so builds are reproducible.

### 2.3 Jenkins plugins

Pipeline, Git, Docker Pipeline, Credentials Binding, Workspace Cleanup. Optionally Blue Ocean or the Pipeline Graph View plugin for readable stage views.

### 2.4 Testing tools

| Tool | What it is | Used for |
|---|---|---|
| `go test -race` | Unit tests with data-race detection | Business logic |
| **testcontainers-go** (or a compose test stack) | Starts throwaway Postgres/Valkey containers from Go tests | Integration tests against real databases |
| **Hurl** | Plain-text HTTP request/assert files | API contract tests run in Jenkins |
| **k6** | Scriptable load-testing tool | Heartbeat, manifest-poll and dashboard load tests |

---

## 3. Shared tasks

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| COM-1 | Add `.gitattributes` to every repo (`*.sh`, `Dockerfile`, `Jenkinsfile`, `*.yml`, `*.conf`, `*.sql` → `eol=lf`); run `git add --renormalize .` | Containers start without `^M` / "no such file" entrypoint errors | — |
| COM-2 | Go service template repo (layout in §3.1): `net/http` server with read/write/idle timeouts, env-var config struct, `slog` JSON logging, `/healthz`, `/readyz`, `/metrics`, internal pprof port, `migrate` subcommand, graceful shutdown on `SIGTERM` via `http.Server.Shutdown` | New service can be created by copying the template in under an hour | — |
| COM-3 | Request ID propagation: Gateway generates `X-Request-Id` if absent; every service logs it and forwards it on outbound calls | One request can be traced across Gateway → service logs by a single ID | COM-2 |
| COM-4 | Shared token-verification middleware: fetch JWKS, cache keys by `kid`, verify `iss`, `aud`, `exp`, `nbf`; configured per route group with exactly one issuer | Unit tests: player token rejected on staff routes and vice versa; expired and wrong-`aud` tokens rejected | COM-2 |
| COM-5 | Standard error body: `{ "error": { "code": "...", "message": "...", "request_id": "..." } }` | All services return this shape for 4xx/5xx | COM-2 |
| COM-6 | Postgres provisioning: one instance, databases `config`, `session`; users `config_rw`, `patch_ro` (read-only on config), `session_rw`. Auth and admin-auth keep their existing databases | Each service can only connect to its own database | — |
| COM-7 | Compose base file: internal network, Postgres, Valkey, registry, named volumes; only Gateway publishes ports | `docker compose up` brings up infrastructure cleanly on a fresh VM | COM-6 |
| COM-8 | Jenkins shared pipeline (Jenkins shared library or a copied Jenkinsfile template): test → build image → tag with git SHA → push → run migrations → deploy → poll `/readyz` → roll back to previous tag on failure | A broken image never stays deployed longer than the readiness timeout | COM-7 |
| COM-9 | Secrets via Jenkins credentials written to env files at deploy time; `.env` files git-ignored | No secrets in any repository | COM-8 |
| COM-10 | Metric and log conventions document: metric names `<service>_<thing>_<unit>`, allowed labels (`route`, `method`, `status`, `channel`), forbidden labels (player ID, staff ID, raw URL, request ID) | Written and linked from every service README | — |
| COM-11 | Go Dockerfile: `golang:1.27.1-bookworm` (or `-alpine`) builder with module cache mount, `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$GIT_SHA"`; runtime image `gcr.io/distroless/static-debian12:nonroot` (or `scratch` + CA certificates), non-root user | Final image under ~25 MB; container runs as non-root | COM-2 |
| COM-12 | Jenkins Go stage: `go vet`, `golangci-lint run`, `govulncheck ./...`, `go test -race ./...`; cache `GOMODCACHE` and `GOCACHE` between builds | Lint, vuln or race failure blocks the image build | COM-8, COM-11 |
| COM-13 | Shared Go module `platform` (in its own repo or a `go.work` monorepo): token middleware (COM-4), request ID (COM-3), error writer (COM-5), metrics middleware (DSH-B1), `pgxpool`/Valkey constructors with health checks | All four services import it instead of copying code | COM-2 |
| COM-14 | Postgres pool sizing: `pgxpool` `MaxConns` set per service so the sum across all services stays below Postgres `max_connections` minus a margin for admin/migrations | Documented connection budget; no "too many clients" errors under k6 load | COM-6, COM-13 |

### 3.1 Go service layout

```
<service>/
  cmd/<service>/main.go      # flag parsing: `serve` (default) or `migrate`
  internal/api/              # HTTP handlers, request/response structs
  internal/store/            # SQL (hand-written or sqlc-generated), Valkey access
  internal/domain/           # business rules with no HTTP or DB imports (easy to unit test)
  migrations/                # goose .sql files, embedded with //go:embed
  Dockerfile
  Jenkinsfile
  go.mod
```

Keep packages few and flat. `internal/` stops other repos from importing service internals; shared code goes in the `platform` module (COM-13).

---

## 4. Admin WebUI shell (shared by Config and Dashboard)

### Tools

| Tool | What it is | Used for |
|---|---|---|
| **Node.js** (LTS) + npm | JavaScript runtime and package manager | Building the frontend (build time only, never shipped) |
| **Ionic Framework** (v9 is current) | UI component library for web and mobile | Admin app components and layout |
| **Angular, React or Vue** | Frontend framework Ionic runs on | App structure. Pick the one your team knows; Angular is Ionic's longest-supported integration. **Chosen for this app: Vue 3** (see "Stack as built" below) |
| **Capacitor** (v8) | Wraps the web build as an Android/iOS app | Native admin app (optional in M1) |
| **Vite** (React/Vue) or Angular CLI | Build tool | Production bundles |
| **ESLint** + **Prettier** | Linting and formatting | CI quality gate |
| **Playwright** | Browser automation | End-to-end smoke tests in Jenkins |

### Tasks

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| WEB-1 | Scaffold Ionic app with the chosen framework; routes `/login`, `/config/*`, `/dashboard/*` | App builds to static files | — |
| WEB-2 | API client module: base URL from runtime config file (`/admin/env.json`), attaches bearer token, maps the COM-5 error shape | No hard-coded URLs in the bundle; same image works in dev and prod | WEB-1 |
| WEB-3 | Staff login + MFA screen against admin-auth | Successful login lands on dashboard; failures show server message | WEB-2, admin-auth |
| WEB-4 | Token handling: access token in memory; single-flight refresh on `401`; logout clears state | Five parallel requests during expiry trigger exactly one refresh call | WEB-3 |
| WEB-5 | Role-gated navigation using `roles` claim (UX only) | `viewer` does not see publish controls | WEB-4 |
| WEB-6 | Dockerfile: multi-stage — Node stage builds, nginx stage serves `dist/` with SPA fallback (`try_files $uri /index.html`), long cache headers on hashed assets, no-cache on `index.html` and `env.json` | Image contains no Node runtime; deep links survive page refresh | WEB-1 |
| WEB-7 | Jenkins pipeline for the WebUI using COM-8 | Push to main deploys a new admin UI | WEB-6, COM-8 |
| WEB-8 | (Optional) Capacitor Android build stage; Gateway CORS allows the Capacitor WebView origin | APK installs and logs in against the VM | WEB-4 |

### What shipped (2026-09-22)

`services/adminui` is the shell for WEB-1 to WEB-7. Its state, task by task:

| ID | State | Where |
|---|---|---|
| WEB-1 | Done | `services/adminui`; `src/router/index.ts` carries `/login`, `/login/mfa`, `/config/*` and `/dashboard/*` |
| WEB-2 | Done | `src/api/env.ts` loads `/admin/env.json` before the app mounts; a missing or malformed file renders a readable failure panel rather than a blank page |
| WEB-3 | Coded against a proposal | `src/views/auth/LoginView.vue`, `src/views/auth/MfaView.vue`. The endpoints are specified in `06-auth-identity-contract.md` §13 and implemented by the Go service `services/admin_auth` (login, refresh, logout, me, onboarding); MFA is still in review |
| WEB-4 | Done | `src/api/client.ts` (token in memory, single-flight refresh); the five-parallel-401 case is in `tests/unit/api-client.spec.ts` |
| WEB-5 | Done | `src/auth/guard.ts`, `src/auth/roles.ts`, `src/components/RoleGate.vue`, and the role-gating case of the E2E smoke |
| WEB-6 | Done | `services/adminui/dockerfile`, `services/adminui/nginx.conf`. Built and run on 2026-09-22 on a machine with Docker, and then checked again through a real `gateway_dev` as its upstream: the non-root runtime, both cache headers, the deep-link fallback and the `Set-Cookie` pass-through on `/admin-auth/` all hold. `services/adminui/README.md` records what was run |
| WEB-7 | Half | `ci/dispatcher/Jenkinsfile`, `ci/services/Jenkinsfile` and `.github/workflows/ci.yml` carry the pipeline. The job `otomo-adminui` has to be created on the CI host. Jenkins is CI for `staging` (the dispatcher accepts only `refs/heads/staging`) and no longer deploys; deploying `main` is outside Jenkins |
| WEB-8 | Not started | Optional in M1, and the only item that would make cross-origin support load-bearing |

### Two proposals and one decision this section did not contain

**`/admin/env.json` has a shape now (proposal).** WEB-2 requires the file, and nothing said what is in it:

```json
{
  "apiBaseUrl": "",
  "appTitle": "Otomo Admin",
  "environment": "live",
  "auth": {
    "loginPath": "/admin-auth/login",
    "refreshPath": "/admin-auth/refresh",
    "logoutPath": "/admin-auth/logout",
    "mfaPath": "/admin-auth/mfa/verify"
  }
}
```

`apiBaseUrl: ""` means same origin, which is what production is: the SPA and the API are both served through `gateway_dev`. Unknown keys are ignored, so a newer file cannot break an older bundle, and a **missing** file fails loudly with a readable error instead of a white page. The image ships this default and a deployment overwrites that one file, which is what makes a single image serve dev, staging and live.

**The staff HTTP surface** in `06-auth-identity-contract.md` §13 is now implemented by `services/admin_auth` (login, refresh, logout, me, onboarding, and `/api/admin/users*`), with the refresh token as an httpOnly cookie scoped to `/admin-auth/`. MFA (`/admin-auth/mfa/verify`) and the audit endpoint are still in review; `src/api/auth.ts` remains the only frontend file that changes if a shape shifts.

**The WebUI is served at `/admin/`, not at the root (decision, and it changes WEB-6's fallback).** `gateway_dev` strips no prefix (`internal/router/dev.go` sets no `StripPrefix`, and `internal/proxy/proxy.go` only rewrites a non-empty one), so a request for `/admin/dashboard` reaches the container still carrying `/admin/`. The app is therefore at `/admin/` throughout: Vite's `base` is `/admin/` and `nginx.conf` serves that path with `try_files $uri $uri/ /admin/index.html`. WEB-6's literal `try_files $uri /index.html` is the form for an app at the origin root, and does not resolve in this layout.

The serve stage is `nginxinc/nginx-unprivileged`, which binds **8080** because it runs as a non-root user. That is what the `-p ${PORT}:8080` every service's job already uses assumes, so this service needs no container-port parameter; the CI port map gives it host port **5010** (5008 is Jenkins's own, 5009 is `gateway_dev`'s).

### Stack as built

The Tools table above leaves the framework open and defers the deep-module libraries to the Config tickets. What the shell actually uses, so the next reader does not have to infer it from `package.json`:

- **Vue 3, with Ionic 9 and Vite.** The router is built by `@ionic/vue-router` rather than `vue-router` directly, because the stack navigator and the router have to agree about the route table.
- **pinia** for the staff session, **CodeMirror 6** for the raw-JSON pane, **@cfworker/json-schema** (draft 2020-12) for validating a namespace against its own schema in the browser; it interprets schemas instead of compiling them with `eval`, so it runs under the admin UI's CSP.
- **Vitest** for the unit tests and **Playwright** for one smoke spec of five cases. No HTTP client (a single `fetch` wrapper, matching the Go side's stdlib-first habit) and no charting library: the time series belong to `DSH-D*`.
- **Not JSON Forms and not jsondiffpatch**, although `02-config.md` names both for CFG-D2 and CFG-D5. The editor's form renderer is hand-rolled over the schema subset the namespaces use (`src/config/controls.ts`, `src/config/presentation.ts`), and `src/config/diff.ts` diffs documents itself, with the reasoning in its header. Those two tickets should be read against what shipped rather than assumed.
- The envelope's `render` block is parsed and then ignored on purpose: it is a hint the shell does not act on, and a field modelled only to be ignored reads as a feature that is not there. Degradation is the property that matters instead: no envelope means schema order with property names as labels, a field named by `order`/`groups` but absent from the schema is ignored, and a field in the schema that neither names is appended to a trailing group. An envelope can never make a field unreachable.
- The tests are unit tests over the pure modules plus that smoke spec. Nothing renders a component in isolation, so `ConflictDialog.vue` and `SchemaField.vue` are covered only through what drives them.
