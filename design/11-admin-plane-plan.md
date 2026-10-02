# Admin plane — review and implementation plan

**Scope:** `gateway_dev` → `admin-auth` (new) → `config` / `patch` / `dashboard` → `adminui`, and the
host it runs on.
**Baseline:** `staging @ 650bba6`, `design/10-communication-schema.md`, and the host
`you@play.example.com` as found on 2026-09-26.
**Status:** partially implemented as of 2026-09-26. In `staging`: GWD-1…5, the admin-auth
service (AA-1…8), Config's handlers and Patch's manifest/blob handlers. Still in review or absent:
MFA (AA-9), admin-auth audit and staff metrics (AA-10), the Dashboard handlers (DSH-*), OPS-1
compose, and the WebUI work.

---

## 1. Where things stand

### 1.1 Per service

| Service | State | What is there | What is missing or wrong |
|---|---|---|---|
| `gateway_dev` | **Done for its current scope** | Separate binary, route table fixed in code, EdDSA pinning, staff JWKS client with fail-closed readiness, `live` publish gated at `admin` by route specificity, stream routes clear the write deadline | (1) `/api/admin/config/` gated at `live_ops`, so a `viewer` can read nothing even though Config allows it. (2) Rate-limit env vars are parsed and never used, and `/admin-auth/login` is about to become a real brute-force surface. (3) No body-size limit, and the 10 s read timeout means a pack upload (up to 512 MB, CFG-B7) cannot get through the edge at all. (4) CORS is parsed and never used; the app is same-origin by design. (5) The upstream is still called `phpadmin` |
| `config` | **Foundation only** | Route table with per-route roles, in-service JWT re-verification, blob store (fan-out paths, temp file + fsync + rename, hash regex guard), full §3 schema + bootstrap channel heads, same-transaction audit writer, readiness (DB + blob root + JWKS) | All 20 handlers answer 501. Not in `deploy/compose.yaml`. `.env.example` has the wrong staff issuer (gap 4 in doc 10) |
| `patch` | **Stub** | `fmt.Println("Patch Test")` | Everything. The player gateway routes `/patch/v1/live/blob/`, but the spec (doc 03 §4) says `/patch/v1/blob/{sha256}`, and the gateway's 30 s write timeout would cut any large download short |
| `dashboard` | **Stub** | `fmt.Println("Dashboard Test")`; its `go.mod` module path (`dashboard/dashboard`) does not follow the repo convention | Everything, plus the whole observability stack (Prometheus, Loki, Alloy, exporters) |
| `admin-auth` | **Does not exist** | Contract proposal in doc 06 §13, which the WebUI is already coded against | The service. The host runs a stand-in (see below) |
| `adminui` | **Architecture good; UI unfinished and poorly laid out** | Runtime `env.json`, single-flight refresh, correct 401/403 split, role-gated nav derived from the route table, MSW fixture API, about 30 unit test files, a Playwright smoke test, nginx image | See §1.2. There are no screens for versions, diffs, packs, releases, rollback/promote, service detail/charts, or user management |

Also: `services/dashboard/dashboard` and `services/patch/patch` are 11 MB compiled binaries sitting
untracked in the source directories. Delete them and add them to `.gitignore`.

### 1.2 Why the WebUI looks bad

I could not screenshot it: this WSL box has no Chromium system libraries and no sudo. The points
below come from reading the templates:

- **Mobile layout on a desktop tool.** Every service on the overview gets its own full-width stacked
  `IonCard`. There is no grid, no table, and no information density, so a 10-service overview is a
  long scroll of cards that each hold one line of text.
- **No design system.** `theme/variables.css` overrides one colour. Spacing, type scale, surfaces and
  status colours are ad hoc per view (11 separate scoped `<style>` blocks).
- **No data visualisation.** The Dashboard spec calls for uPlot charts and virtualised logs. Neither
  library is installed; the overview shows numbers as sentences.
- **Missing workflows.** The Config module stops at the draft editor, so half of what the spec
  describes has no screen.
- The app shell is a default `IonMenu` list: no section grouping, no environment banner, and the
  identity block sits mid-sidebar.

Ionic is mandated by `00-common-stack.md` (with an optional Capacitor build, WEB-8), so the plan
restyles it rather than replacing it. Ionic provides the chrome, inputs and overlays; the layout
underneath is ours.

### 1.3 The host (`play.example.com`)

2 vCPU, 3.7 GiB RAM (2.8 GiB available), 24 GB of disk free. It already runs Jenkins (330 MB) and a
game server.

The running stack was assembled by hand and is **not** what `deploy/compose.yaml` describes:

- `~/otomo` is not a git checkout.
- `config`, `session` and `admin-ui` run as `:test` images outside compose.
- A second Postgres (`otomo-pg`) is bound to `127.0.0.1:5432`.
- `php-admin-auth` is `~/otomo-testauth`, a stand-in that **mints an admin token for any
  credentials**. It is safe only because `gateway_dev` is loopback-bound.
- `dashboard` is `~/otomo-dashboard-real/dashboard.mjs`, a Node prototype that reads Docker json-file
  logs as root. It is useful as a reference for log parsing, and it has to go.

`gateway_dev` is published on `127.0.0.1:8090`. **I verified the SSH tunnel today:**
`ssh -L 18090:127.0.0.1:8090 you@…` gives `/admin/ → 200` and
`/api/admin/config/namespaces → 401 missing_token`. The access model you want already works at the
network layer. What is missing is a real identity behind it.

---

## 2. Target shape

```
 laptop ── ssh -L 8090:127.0.0.1:8090 ──► host 127.0.0.1:8090
                                              │
                                        gateway_dev  (rate limit, body caps, staff JWT, role gates)
   ┌───────────────┬──────────────┬───────────┴───┬──────────────────┬──────────────┐
 /admin/        /admin-auth/*   /api/admin/users/*  /api/admin/config/*  /api/admin/dashboard/*
 adminui        admin-auth ◄────────┘               config               dashboard
 (nginx)        (Go, new)                             │  │                 │   │   │
                  │ JWKS ◄── gateway_dev, config,     │  └─ blobs volume ──┼───┼── patch (ro)
                  │          session, dashboard       │                    │   │
                  └── pg: admin_auth               pg: config ◄─ patch_ro  │   └─ REST /audit fan-out
                                                    NOTIFY config_release ─┘      (config, admin-auth)
                                                                          Prometheus · Loki ◄ Alloy
```

`admin-auth` owns staff identities, signing keys, sessions, invites and its own audit log. It serves
two surfaces through one process:

- `/admin-auth/*`: public group, the login surface.
- `/api/admin/users/*`: staff group, gated at `admin` **in `gateway_dev`'s route table**, and
  re-checked in the service.

Both are the same path space, forwarded unstripped, the same pattern Session uses for its two
prefixes.

---

## 3. Decisions I need from you

Each one has a recommendation. The task list below assumes the recommendation, and the text says
where a different answer would change it.

| # | Decision | Recommendation | Why |
|---|---|---|---|
| D1 | admin-auth implementation | **Go, `services/admin_auth`**, same skeleton as `auth` (`serve / migrate / genkey / bootstrap-root`), keeping doc 06 §13's wire contract unchanged | The WebUI's auth layer then needs zero changes. One language and one JWT validator across the repo. "PHP Admin Auth" is renamed "admin-auth" throughout the docs |
| D2 | Role model | Keep **`viewer < live_ops < admin`** as the only token roles. "root" is an **account flag**, not a fourth role: an `admin` that cannot be disabled, demoted or deleted, created only by `bootstrap-root` | Adding a fourth role touches every validator's rank table for no authorization gain |
| D3 | Who may grant what | Anyone `admin` may invite `viewer` or `live_ops`. **Granting `admin` requires root.** Nobody may change their own roles | Limits the damage a single compromised admin account can do |
| D4 | Onboarding mechanism | **One-time invite link.** An admin creates an invite (email, name, role), the server returns the URL once (72 h expiry, stored hashed), and the admin sends it out-of-band. The invitee opens it through the tunnel and sets a password | There is no mail server, and a link never puts a password in anyone's hands. The same mechanism covers password resets |
| D5 | MFA | **TOTP**, required for `admin` accounts and optional for others; implemented after password login works (AA-9). Root is exempt and password-only | Root is a shared break-glass account whose credential lives in a file on the host, so a TOTP secret on it would be shared too. Root's only job is to create personal admin accounts |
| D6 | Root password | Generated by `deploy/scripts/generate-secrets.sh` into `deploy/secrets/admin_root_password` (mode 0600, owned by the deploy user), read by `bootstrap-root`. Admins read it over SSH; `admin-auth bootstrap-root --rotate` replaces it and revokes root's sessions | The password is only ever available to people who can already SSH in, which is the same trust boundary as the tunnel |
| D7 | Dashboard data source | **As specified in doc 01:** Prometheus + Loki + Alloy + node_exporter + cAdvisor, with compose memory limits (about 600 MB in total) | Fits in 2.8 GB available. The lean alternative (Dashboard scrapes `/metrics` itself into memory and reads Docker logs) saves about 500 MB but contradicts doc 01's key design decision. Because the Dashboard is written behind `MetricsSource` and `LogSource` interfaces, this stays reversible |
| D8 | Config read gate on `gateway_dev` | Lower `/api/admin/config/` to **`viewer`**. Config already enforces per-route roles (writes are `live_ops` or `admin`) | Closes gap 1. The literal-path `admin` gate on the live release route stays |
| D9 | Patch blob serving | **Go `http.ServeContent`** in the patch service over a read-only mount of the blob volume, instead of an nginx sidecar. Nginx `sendfile` stays as a later optimisation | Every byte crosses the gateway's Go proxy anyway, so a sidecar only saves one hop. `ServeContent` already handles `Range`, `If-None-Match` and 206 responses. It is one container fewer on a small host |
| D10 | Host cutover | Replace the hand-built stack with a git checkout plus `deploy/scripts/up.sh`; delete `otomo-testauth`, `otomo-dashboard-real`, the `:test` containers and `otomo-pg` | This is **destructive on a shared host**. Confirm that nobody else depends on `otomo-pg` or the stand-ins before OPS-5 |

---

## 4. Tasks

IDs are new except where they extend an existing spec ID, which is named in the "Spec" column. The
order follows your ask: the edge first, then upstream, then the frontend.

### Phase G — `gateway_dev`

| ID | Task | Acceptance criteria | Spec | Depends on |
|---|---|---|---|---|
| GWD-1 | Rename upstream `phpadmin` → `adminauth`; add route `* /api/admin/users/` → `adminauth`, `GroupStaff`, `MinRole: admin`; lower `/api/admin/config/` to `viewer` (D8) | A viewer token reads namespaces (501/200, not 403). A live_ops token on `/api/admin/users` gets 403. Route table tests are updated | gap 1 | — |
| GWD-2 | Per-IP token-bucket limiter using the existing `RATE_LIMIT_*` vars (keyed on `RemoteAddr`, as in `gateway`), plus a stricter bucket on `/admin-auth/login`, `/admin-auth/mfa/verify` and `/admin-auth/onboard` applied before auth | The 11th login in 1 s returns 429 COM-5. Static `/admin/` assets are not limited | gap 10 | — |
| GWD-3 | Per-route `MaxBody` and `Upload` fields. The default body cap is 1 MiB. `POST /api/admin/config/packs` gets 512 MiB with its read deadline cleared (`ResponseController.SetReadDeadline(time.Time{})`) | A 300 MB upload passes through the gateway with flat RSS. A 2 MB JSON body to any other route gets 413 COM-5 | gap 12, CFG-B7 | — |
| GWD-4 | Delete the CORS config (same-origin by design, doc 06 §13.2) and its env var; update the README and `.env.example` | Boot fails nowhere; no dead config remains | gap 12 | — |
| GWD-5 | Set `X-Forwarded-Proto` and `X-Forwarded-For` for admin-auth only, so it can log the client IP and rate-limit per account | admin-auth logs show the real client address | — | GWD-1 |
| GWD-6 | Tests: route-policy table test extended for users/viewer/upload/limits; smoke script updated | `go test ./...` and `smoke.sh` are green | — | GWD-1…5 |

### Phase AA — `admin-auth` (new service)

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| AA-1 | Scaffold `services/admin_auth` from `auth`'s shape: public `:8080` plus internal `:9090` (healthz, readyz, metrics, pprof), COM-5 errors, JSON slog, goose migrations, `go.work` entry | `/readyz` = DB + an active signing key | GWD-1 |
| AA-2 | Schema: `staff_user` (uuid, `email citext unique`, name, argon2id hash, `roles text[]`, `is_root`, `status`, `totp_secret_enc`, `failed_logins`, `locked_until`, timestamps), `staff_invite` (token_hash, email, name, role, purpose `invite\|reset`, created_by, expires_at, used_at), `refresh_session` (family_id, token_hash, user_id, expires_at, revoked_at, ip, user_agent), `signing_key`, `audit_log` (same shape as Config's) | Migrations are idempotent | AA-1 |
| AA-3 | Signing: Ed25519 key file plus a DB row, `genkey`, and a JWKS endpoint served from a pre-marshalled atomic pointer (copied from `auth/internal/token`). Claims per doc 06 §5: `iss=https://admin-auth.otomo.internal`, `aud=["otomo:staff"]`, `sub`, `roles`, and 15 min `exp` | gateway_dev, config and session validate the token with no change to their code | AA-2 |
| AA-4 | `POST /admin-auth/login`: argon2id verify, dummy-hash on unknown email (constant time), per-account lockout (5 failures → 15 min), generic `invalid_credentials` message; returns doc 06 §13.1 body plus the `__Host-otomo_refresh` cookie (`HttpOnly; Secure; SameSite=Strict; Path=/`, no `Domain`) | A wrong password and an unknown email are indistinguishable by status, body and timing (±10 ms). The 6th failure is locked | AA-3 |
| AA-5 | `refresh` (rotation, reuse revokes the whole family, **roles re-read from the DB**), `logout`, `me` | Demoting a user takes effect by the next refresh. A replayed refresh token kills every session in its family | AA-4 |
| AA-6 | `bootstrap-root` subcommand (D6): creates or rotates root from a secret file; idempotent; refuses to run without the file | Running it twice is a no-op; `--rotate` changes the password and revokes root's sessions | AA-2 |
| AA-7 | User management under `/api/admin/users` (admin): `GET` list, `POST` invite `{email,name,role}` → `{invite_url, expires_at}` shown once, `GET /invites`, `DELETE /invites/{id}`, `PATCH /{id}` `{roles?, status?}`, `POST /{id}/reset` → reset link. D3 rules are enforced in the handler; disabling a user revokes their sessions | An admin granting `admin` gets 403. Root cannot be disabled. Every action writes an audit row in the same transaction | AA-5 |
| AA-8 | Onboarding: `GET /admin-auth/onboard/{token}` → `{email,name,role,purpose}`; `POST /admin-auth/onboard` `{token,password}` → consumes the invite and signs in. Password policy: at least 12 characters and checked against a bundled common-password list | A token cannot be used twice or after it expires. The link works through the tunnel | AA-7 |
| AA-9 | TOTP (D5): `POST /admin-auth/mfa/enroll` + `confirm`, `mfa/verify` with an opaque short-lived ticket, and 10 single-use recovery codes. Enrollment is enforced on an admin's first login | An admin without TOTP is forced through enrollment. A code cannot be replayed within its time step | AA-5 |
| AA-10 | `GET /admin-auth/audit` in the shared audit shape (closes gap 7), plus metrics `staff_login_total{result}` and `staff_refresh_total{result}` | Dashboard audit merge sees logins and user changes | AA-7 |
| AA-11 | Tests: unit (hashing, lockout, rotation, D3 matrix) and integration against real Postgres (concurrent refresh with the same token → exactly one succeeds) | Green in CI | AA-1…10 |

### Phase C — Config end to end (the master data pipeline)

These are doc 02's Phase B tasks, with the concurrency additions marked **Δ**.

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-B1 | Namespaces: list (with latest version and draft status in one query) and create (admin). Creating a namespace also creates schema v1 `{}` and an empty draft in the same transaction | Response matches doc 10 §9 row 6 | — |
| CFG-B2 | Schema GET/PUT: compile with `santhosh-tekuri/jsonschema` (draft 2020-12) before insert; next `schema_version` allocated under `SELECT … FOR UPDATE` on the namespace row. Compiled validator cache keyed `(ns, schema_version)` behind an `RWMutex` | Invalid schema → 400. Two concurrent PUTs → versions n+1 and n+2, never a collision | B1 |
| CFG-B3 | Draft GET/PUT: `UPDATE … WHERE revision=$2`; zero rows → 409 `stale_revision` with the current draft in `details`. Strict JSON decoding (duplicate keys and invalid UTF-8 rejected) | Concurrent-save test: exactly one 200 and one 409 | B1 |
| CFG-B4 | `draft/validate` → `{valid, errors:[{pointer,message}]}` | Pointers match what the UI's `validation.ts` expects | B2 |
| CFG-B5 | Create version. **Δ** The request carries `{message, revision}` and fails with 409 if the draft moved since the user reviewed it. The draft row is locked `FOR UPDATE`, re-validated, canonicalised (RFC 8785 via `jsontext`), hashed, and **the canonical bytes are written to the blob store before the insert** | An invalid draft cannot be versioned. The same content yields the same sha256 regardless of key order | B4 |
| CFG-B6 | Versions list/get and structural diff (`from`/`to` ∈ version \| `draft`) | Array reorder and nested changes are reported | B5 |
| CFG-B7 | Pack upload: `Peek(4)=="GDPC"`, streamed through `MultiWriter(tmp, sha256)` under `MaxBytesReader`, `ON CONFLICT (sha256) DO NOTHING`; list packs | A 400 MB upload keeps RSS flat; the same pack uploaded twice is one row and one blob | GWD-3 |
| CFG-B8 | Publish. **Δ** The body carries `base_release_id` (what the composer previewed), and a moved head gives 409 `stale_release`. One transaction: lock `channel_head FOR UPDATE`, resolve versions, drop `server` namespaces, verify every blob exists, build and canonicalise the manifest, insert the release, move the head, write audit, `pg_notify('config_release', ch)` | Two simultaneous publishes serialise. A server-audience namespace never appears in any client manifest (unit test) | B5, B7 |
| CFG-B9 | Release history, rollback, promote (`staging → live`), and `GET /audit` with a keyset cursor | Rollback sends a notify and Patch serves the old manifest | B8 |
| CFG-B11 | Metrics: `config_publish_total{channel}`, `config_validation_failures_total`, `config_pack_upload_bytes_total` | Visible in Prometheus | B8 |
| CFG-C1 | Seed namespaces and schemas in a repo folder (`features.flags`, `balance.player`, `ui.motd`, `ui.presentation`) plus a `config seed` subcommand | A fresh stack has something to edit | B2 |
| CFG-E2 | Integration tests against real Postgres with `-race`: concurrent draft saves, concurrent publishes, the version-while-editing race, audit-in-same-transaction rollback | Green in CI | B3, B5, B8 |

### Phase P — Patch (start) and read/write protection

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| PAT-B1 | Scaffold (same template, `patch_ro` DB role, blob volume mounted **read-only**) | `/readyz` is 503 until the first full load | — |
| PAT-B2 | Load all channel heads into an immutable `manifestSet` behind `atomic.Pointer`; handlers do one load, one ETag compare, one write | No lock on the read path; `-race` is clean under a concurrent reload | B1, CFG-B8 |
| PAT-B3/B4 | `LISTEN config_release` on a hijacked connection. On notify, reload the channel. Poll heads every 60 s and reload everything on reconnect | A new manifest is served within 5 s of publish. Killing the listener leaves nothing stale for more than 60 s | B2 |
| PAT-B5 | `GET /patch/v1/{channel}/manifest`: ETag, `If-None-Match` (list and `*`), 304, `X-Min-Client-Version`; dev/staging require a staff token re-verified in the service | 304 with zero body bytes on a match | B2 |
| PAT-A2′ | `GET /patch/v1/blob/{sha256}` via `http.ServeContent` (D9). The regex is checked before any filesystem access; immutable cache headers; Range support | `../` → 400. `curl -r 0-1023` → 206 | B1 |
| PAT-G1 | Player `gateway`: replace `/patch/v1/live/blob/` with `GET /patch/v1/blob/`, marked `Stream` so the 30 s write deadline does not cut a download | 400 MB download through the gateway without the gateway's memory growing | A2′ |
| PAT-D2 | Integration test: publish → manifest changes; rollback → previous manifest returns | Green in the compose test stack | B3, CFG-B9 |

The Godot client updater (PAT-C*) is out of scope here.

**Read/write protection, in one place.** No two writers can silently clobber each other, and no
reader can ever see a half-written state:

| Resource | Writers | Readers | Guarantee |
|---|---|---|---|
| Draft | Optimistic `revision` (B3) | Plain SELECT | A concurrent save gets 409, never a silent overwrite |
| Schema version, config version | Row lock on the parent plus PK uniqueness (B2, B5) | Immutable rows | Version numbers are gap-free and never reused |
| Version ← draft | `revision` in the request (B5 Δ) | — | You version exactly what you reviewed |
| Channel head | `FOR UPDATE` plus `base_release_id` (B8 Δ) | Patch reads through NOTIFY/poll | Publishes serialise, and the diff you confirmed is the diff that ships |
| Blobs | temp + fsync + rename, content-addressed, written before commit, never deleted in M1 | nginx/Patch through a read-only mount | A reader sees a whole file or none; a release never points at a missing blob |
| Served manifest | Patch builds a new set and swaps the pointer | Lock-free atomic load | No request sees a mix of two releases |
| Audit | Same transaction as the change | REST only | No change without an audit row, and no audit row without its change |

### Phase D — Dashboard

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-A1–A4 | Compose: Prometheus (7 d / size cap), Loki single binary (72 h), Alloy (Docker discovery, JSON parse, labels `service` and `level` only), node_exporter, cAdvisor, postgres_exporter. Memory limits on each. **No ports published** | Every service's `:9090/metrics` is scraped; logs are queryable by service | OPS-1 |
| DSH-A5 | CI: `promtool check config`, `alloy fmt` | A bad config fails the build | A1–A3 |
| DSH-C1/C2 | Scaffold the service with repo-convention module path; re-verify the staff JWT (viewer) | A direct call without a token gets 401 | AA-3 |
| DSH-C3/C4 | Named PromQL template catalogue (rps by status class, error ratio, p50/p95/p99, container CPU/mem, **Go runtime: goroutines, heap in use, GC pause p99**). The service allow-list is taken from Prometheus targets; columnar conversion | Unknown template or service → 400. No client string reaches PromQL | C1 |
| DSH-C5/C7 | `/overview`: concurrent instant queries under a 2 s deadline, 10 s singleflight cache, degraded cards instead of a failed page | 10 concurrent requests → one set of queries | C4 |
| DSH-H1 | **Health prober (new).** Every 15 s, GET each service's internal `/readyz` and record `up`, `ready`, the reason string from the 503 body, and latency. Exposed in `/services` and as `otomo_service_ready{service}` | Stopping a container turns its card red within 30 s; a DB outage shows "not_ready: database …" rather than just "down" | C1 |
| DSH-C6 | `/services/{name}/series`: range clamped to 7 d, step chosen so there are at most 1500 points | A 30 d request is clamped | C4 |
| DSH-C8/C9 | `/logs` (LogQL from validated labels, `contains` escaped, limit ≤ 1000, `request_id` filter) and `/logs/tail` SSE (tail caps 2 per user and 10 global → 429; 15 s keep-alive comment) | An injection string in `contains` is treated as literal. A 10-minute tail survives the gateway | C1 |
| DSH-C11 | `/audit`: fan out to Config and admin-auth with the caller's token; k-way merge by `at`; composite cursor | Entries interleave correctly across sources | CFG-B9, AA-10 |
| DSH-C12/E1 | Own metrics; fixture-based unit tests for templates, step calculation and conversion | Green in CI | C1–C11 |

pprof stays on the internal listener and is **not** proxied. "Debug stats" are the runtime
templates above.

### Phase U — WebUI

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| UI-1 | **Design system.** Token file (colour roles light/dark, status colours, 4 px spacing scale, type scale, radii, elevation) mapped onto Ionic's CSS variables; layout primitives (`PageHeader`, `Toolbar`, `Grid`, `Panel`, `DataTable`, `StatTile`, `StatusDot`, `EmptyState`, `ErrorPanel`, `CopyableId`); a 1280 px-first layout that still works at 400 px | Every existing view is rebuilt on the primitives; no per-view ad hoc colours | — |
| UI-2 | **Shell.** Grouped sidebar (Operate: Overview, Services, Logs, Audit · Content: Namespaces, Packs, Releases · Admin: Users), environment banner from `env.json` (red for `live`), identity and roles in the sidebar footer, breadcrumb header, ⌘K route palette | The nav still derives from the route table | UI-1 |
| UI-3 | **Overview redesign:** host strip (CPU/mem/disk tiles), a service grid (status dot, ready reason, rps, err %, p95, version), online players, 15 s poll paused on hidden tab (DSH-D1/D2) | 10 services fit on one 1440×900 screen | UI-1, DSH-C5, H1 |
| UI-4 | Service detail page with uPlot charts (traffic, latency, errors, CPU/mem, Go runtime) and a time-range picker in the URL (DSH-D3/D4) | 1500 × 4 points render without lag; the URL is shareable | UI-1, DSH-C6 |
| UI-5 | Logs: virtualised list (TanStack Virtual), JSON expand, click a `request_id` to filter by it, live tail with scroll-pause and a 5 000-line cap (DSH-D5/D6) | 10 k lines scroll smoothly | UI-1, DSH-C8/C9 |
| UI-6 | Audit table across sources, with a link from `release.publish` to its diff (DSH-D7) | — | UI-1, DSH-C11 |
| UI-7 | Config: namespace list as a table; **create namespace** (admin); **schema editor** (admin, CodeMirror + compile errors) | Matches API state | UI-1, CFG-B1/B2 |
| UI-8 | Config: editor polish, **versions tab** with a diff viewer, **create-version dialog** (message required, sends `revision`) | Versioning a draft someone else changed shows the conflict dialog | CFG-B5/B6 |
| UI-9 | **Packs** page: upload with a progress bar (XHR `upload.onprogress`), resulting hash and size, list | A 100 MB upload shows progress | CFG-B7, GWD-3 |
| UI-10 | **Release composer** per channel: version per namespace (defaults to latest), packs, `min_client_version`, manifest diff against the head, message; `live` requires typing `live`; sends `base_release_id` | A stale head shows "someone published since you opened this" | CFG-B8 |
| UI-11 | **Release history** with rollback and promote (admin only via `RoleGate`) | Rollback updates the view immediately | CFG-B9 |
| UI-12 | **Users** (admin): table with role, status, last login; invite dialog → one-time link with a copy button and an expiry notice; revoke invite; change role (D3 rules mirrored); disable; reset link | A live_ops user never sees the page; an admin cannot pick `admin` unless they are root | AA-7 |
| UI-13 | **Onboarding** route `/admin/onboard/:token` (public): shows who is being invited and at what role, then password + confirm with a strength meter → signed in. TOTP enrollment screen (QR code + recovery codes) for AA-9 | The full invite → first login flow works through the tunnel | AA-8, AA-9 |
| UI-14 | Account page: change password, manage TOTP, sign out other sessions | — | AA-5, AA-9 |
| UI-15 | nginx security headers on `/admin/`: CSP (`default-src 'self'`), `frame-ancestors 'none'`, `Referrer-Policy`, `X-Content-Type-Options` | Checked with `curl -I` | — |
| UI-16 | MSW fixtures and handlers for every new endpoint; unit tests; Playwright flows for sign-in, invite → onboard, draft → version → publish → rollback | Green in CI | all UI |

### Phase O — Deployment, root user, tunnel

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| OPS-1 | Compose: add `admin-auth(-migrate)`, `config(-migrate)`, `patch`, `dashboard`, `admin-ui`, a `blobs` volume (rw for config, ro for patch), and the observability services. Staff JWKS → `http://admin-auth:8080/.well-known/jwks.json`; issuer pinned everywhere (closes gap 4) | `up.sh` brings up the whole admin plane on a fresh VM | AA-3, CFG-B1, PAT-B1, DSH-C1 |
| OPS-2 | Provisioning: `admin_auth` DB and `admin_auth_rw` role in `01-provision.sh`, **plus an idempotent `provision-upgrade.sh`** for hosts whose data directory already exists (the init script never re-runs on them) | Works on both a fresh host and the current one | — |
| OPS-3 | `generate-secrets.sh`: admin-auth signing key, root password file (D6), new DB passwords; `up.sh` runs `genkey` and `bootstrap-root` once | Root can sign in straight after `up.sh` | AA-6 |
| OPS-4 | `deploy/scripts/tunnel.sh` (`ssh -N -L 8090:127.0.0.1:8090 you@play.example.com`) and README instructions: open `http://localhost:8090/admin/`, where to read the root password, first-login steps. Notes: `Secure` cookies are accepted on `http://localhost` by Chrome and Firefox; Safari is unsupported for the tunnel | A new admin can go from zero to signed in by following the README | OPS-3 |
| OPS-5 | **Host cutover (D10, needs sign-off):** git checkout at `~/otomo`, stop and remove the hand-built containers and stand-ins, `up.sh`, verify, then remove `~/otomo-testauth` and `~/otomo-dashboard-real` | `docker ps` matches `compose ps`; the stand-in that mints admin tokens for anyone is gone | OPS-1…4 |
| OPS-6 | Jenkins jobs for admin-auth, patch and dashboard (COM-8 template); doc updates (rename PHP → admin-auth in 00/01/05/06/10; doc 10's packets for the new routes) | Docs match the code | — |

---

## 5. Order of work

```
1  GWD-1..6 ─┐
2  AA-1..8, AA-10/11 ─┬─ OPS-2/3 ─ OPS-1(partial) ─ OPS-4   ◄ milestone: real root login through the tunnel
3  CFG-B1..B9, B11, C1, E2 ── PAT-B1..B5, A2′, G1, D2       ◄ milestone: author → version → publish → Patch serves it
4  DSH-A*, C*, H1                                            ◄ milestone: health, logs, audit are real
5  UI-1/2 (can start in parallel with 2) → UI-3..16          ◄ milestone: every workflow has a screen
6  AA-9 (TOTP), OPS-5 cutover, OPS-6
```

UI-1 and UI-2 depend on nothing in the backend and can run in parallel with step 2. Each later UI
task starts once its endpoint returns something other than 501. Until then, MSW fixtures stand in
for it.

## 6. Out of scope

Godot updater (PAT-C*), manifest signing (CFG-B12), Session's admin surface (including gap 11's
disband role mismatch), alerting, blob garbage collection, and CI for the observability stack beyond
DSH-A5.
