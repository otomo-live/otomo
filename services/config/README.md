# Config

Live-ops configuration service. Owns the namespaces, schemas, drafts, versions, content
packs and per-channel releases that staff edit and that Patch serves to the Godot client.

**What exists today is the foundation:** config, both HTTP listeners, structured logging,
metrics, graceful shutdown, the §3 schema (CFG-A2), content-addressed blob storage
(CFG-A3), the audit writer (CFG-A4), staff-token verification with per-route role
enforcement (CFG-A1, CFG-B10), and the **whole** §5 route table registered at its external
paths. Every business handler behind that table is a `501 not_implemented` stub — Phase B
(CFG-B1…CFG-B12) is not implemented. See "Scope" at the bottom.

Source of truth for this service: `design/02-config.md` (schema §3, routes §5, manifest and
hashing rules), `design/00-common-stack.md` (COM-1…COM-14), and
`design/06-auth-identity-contract.md` §3 and §5 (the staff identity domain and the
`viewer < live_ops < admin` role ladder).

---

## Running it

```sh
export CONFIG_DATABASE_URL='postgres://config_rw:pw@localhost:5432/config'
export CONFIG_BLOB_ROOT=./.blobs
export CONFIG_STAFF_JWKS_URL=http://localhost:8081/.well-known/jwks.json
export CONFIG_STAFF_ISSUER=https://php-admin.otomo.internal
export CONFIG_STAFF_AUDIENCE=otomo:staff

go run . migrate     # create the §3 tables and seed one bootstrap release per channel
go run . seed        # create the embedded starter namespaces (existing ones are skipped)
go run . serve       # or just: go run .
```

`migrate` is idempotent — Jenkins runs it before every deploy, so applying it twice is a
no-op rather than a second set of bootstrap releases. `serve` refuses to start if the blob
root is missing or unwritable, rather than discovering that at the first publish.

```sh
docker run --rm --env-file config.env otomo-config:staging migrate
docker run --rm --env-file config.env -p 8080:8080 -v otomo-blobs:/var/lib/otomo/blobs otomo-config:staging serve
```

### Commands

| Command | Does |
|---|---|
| `serve` (default) | Runs both listeners until `SIGTERM`/`SIGINT`, then shuts down gracefully. |
| `migrate` | Applies the embedded goose migrations. Idempotent. |
| `seed [--dry-run]` | Creates the embedded starter namespaces, skipping any that already exist. `--dry-run` reports what would be created and writes nothing. |

Unknown commands print usage and exit `2`. A configuration error prints **every** problem
at once and exits `1`.

---

## Configuration

Every variable is read once at startup. `CONFIG_DATABASE_URL` is required by both
commands; the three `CONFIG_STAFF_*` variables and `CONFIG_BLOB_ROOT` are required by
`serve` only. The process exits `1` at boot rather than starting in a half-working state —
including when Postgres is unreachable. Compose's restart policy is the retry loop; a
service that cannot reach its database has nothing useful to serve.

See `.env.example` for a copy-pasteable template.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `CONFIG_LISTEN_ADDR` | no | `:8080` | Public listener. Gateway proxies `/api/admin/config/*` here, prefix intact. |
| `CONFIG_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `CONFIG_DATABASE_URL` | **yes** | — | `postgres://config_rw:...@postgres:5432/config` |
| `CONFIG_DB_MAX_CONNS` | no | `8` | See the connection budget below. |
| `CONFIG_BLOB_ROOT` | **yes** (`serve`) | `/var/lib/otomo/blobs` | Content-addressed store, fanned out as `blobs/ab/cd/abcd…`. Must be a volume writable by uid 65532. |
| `CONFIG_STAFF_JWKS_URL` | **yes** (`serve`) | — | PHP Admin Auth's JWKS — **not** Auth's, which issues player tokens. Validated as an absolute URL at boot. |
| `CONFIG_STAFF_ISSUER` | **yes** (`serve`) | — | Checked against the token's own `iss`. |
| `CONFIG_STAFF_AUDIENCE` | **yes** (`serve`) | — | `otomo:staff`. Checked against the token's own `aud`. |
| `CONFIG_STAFF_JWKS_REFRESH` | no | `30s` | How often the key set is re-fetched. |
| `CONFIG_JWT_CLOCK_SKEW` | no | `30s` | Leeway on `exp`/`nbf`. **`0` is accepted** — unlike the timeouts, where `0` would mean "no timeout" rather than "no leeway". |
| `CONFIG_READ_TIMEOUT` | no | `10s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `CONFIG_WRITE_TIMEOUT` | no | `30s` | |
| `CONFIG_IDLE_TIMEOUT` | no | `120s` | |
| `CONFIG_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by both listeners. |
| `CONFIG_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `CONFIG_TEST_DATABASE_URL` | no | — | Read only by the `internal/store` tests. Never set in a deployment. |

### Connection budget (COM-14)

`CONFIG_DB_MAX_CONNS` defaults to `8`. Keep the sum of every service's `MaxConns` below
Postgres's `max_connections`, minus a margin for admin sessions and the one-shot `migrate`
container. Raise it only alongside the Postgres setting.

---

## HTTP surface

### Public listener (`CONFIG_LISTEN_ADDR`)

The route table is `internal/api/routes.go`, in `design/02-config.md` §5's order, and it is
the single place where a path and its required role are stated together. All 19 routes are
registered; all of them answer `501 not_implemented` today. Registering the table before
the handlers is deliberate: the role boundaries are part of the external contract, and a
route that exists but is unimplemented is a much smaller problem than one that is
implemented but reachable by the wrong role.

| Role | Routes |
|---|---|
| `viewer` | every `GET`: namespaces, schema, draft, versions, diff, packs, release history, audit |
| `live_ops` | create namespace, replace schema, save draft, validate draft, create version, upload pack, publish to `dev`/`staging` |
| `admin` | publish to `live`, rollback, promote |

`POST /api/admin/config/channels/live/releases` is a **literal** path registered alongside
the `{ch}` wildcard, and ServeMux prefers the more specific pattern — so "publishing to
live needs admin" is expressed in the table rather than in a handler. Gateway's route
table states the same two entries.

Paths are the **external** path, prefix included: Gateway forwards
`/api/admin/config/*` without stripping it, the same convention `/auth/*` follows. Nothing
under that prefix is public — including the fallback, which is guarded as `viewer` too, so
an unauthenticated request for a path that does not exist is answered exactly like one for
a path that does, and the service cannot be used to enumerate its own route table.

Middleware, outermost first: request ID → access log → metrics → `recover` → the route's
token+role check → mux. The order is load-bearing: the request ID has to be outermost so
the recovery handler can put it in a `500` body, and `recover` has to be inside the log and
metrics so a panicking request is still counted and logged with its real status.

### Internal listener (`CONFIG_METRICS_ADDR`)

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200 ok` once the process is listening. |
| `GET /readyz` | `200` once startup finished **and** Postgres answers, the blob root is writable, and the staff JWKS has been fetched; `503` otherwise, naming the dependency that is not ready. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

The Postgres ping behind `/readyz` is cached for 10s, so probes do not become database
load. None of these routes exist on the public listener — they return the `404` body below.
There is no authentication on this listener: the port is not published and nothing routes
to it, and that isolation is the whole access-control story.

### Error shape (COM-5)

Every 4xx and 5xx from the public listener, including `404` and `405`:

```json
{ "error": { "code": "not_found", "message": "...", "request_id": "..." } }
```

`request_id` echoes the inbound `X-Request-Id`, or a fresh UUIDv4 if Gateway did not supply
one; the same value is returned in the `X-Request-Id` response header. `api.WriteError` is
the only thing that writes an error status.

A rejected token uses the same envelope, with `code` set to the rejection reason and `401`
(`403` for `insufficient_role`). The seven reasons — `missing_token`, `invalid_signature`,
`expired`, `aud_mismatch`, `iss_mismatch`, `insufficient_role`, `invalid_token` — are
deliberately the same strings as Gateway's, so a request Gateway admitted and Config
refused names its cause the same way in both services' logs. One is worth calling out
because it is not the intuitive guess: while PHP Admin Auth is unreachable, a valid-looking
token is rejected as `invalid_signature`, not `invalid_token` — with the key set empty, its
`kid` resolves to nothing and the token is unverifiable, which is the honest description of
what happened.

### Schema replace precondition (CFG-B2)

`PUT /api/admin/config/namespaces/{ns}/schema` is guarded by optimistic locking on the
schema version. The client presents the version it edited as `If-Match: "<n>"` (quoted or
bare) or as `?base_version=<n>`; sending both is allowed only when they agree. A missing
precondition is `428 precondition_required` ("send the schema version you edited
(If-Match)"), a malformed or conflicting one is `400 validation_failed`, and a weak
`W/"3"` validator is rejected. `GET` returns the current version as `ETag: "<n>"`, so the
normal flow is to read it, edit, then echo it back in `If-Match`.

When the version has moved on, the replace is refused with `409 stale_schema` and the
message `schema v{current} was saved since v{expected}`; the body's `details.schema` is the
current schema response object, so the editor can reconcile without a second round trip.
A successful replace is `200` with the new schema version.

### Metrics and logs (COM-10)

Metric names are `config_<thing>_<unit>`, labels are limited to `route`, `method`, `status`,
`reason` and the three fixed channels `dev`/`staging`/`live`:

- `config_http_requests_total{route,method,status}`
- `config_http_request_duration_seconds{route,method}`
- `config_token_rejected_total{reason}`
- `config_build_info{version}`
- `config_publish_total{channel}` — successful publishes, by channel; `dev`, `staging` and `live` are pre-initialised at 0. Rollback and promote are not publishes and do not count.
- `config_validation_failures_total` — documents found invalid against their namespace schema, from `draft/validate` answering `valid:false` and from version creation rejected with schema issues. A malformed request is not a schema failure and does not count.
- `config_pack_upload_bytes_total` — bytes of content packs accepted by successful uploads.

`route` is the path part of the **ServeMux pattern** that matched, never the raw URL, so it
stays bounded in cardinality and cannot leak identifiers. The method is already its own
label, so the pattern's `POST ` prefix is stripped; a request the fallback answered —
including an unmatched path and every `404`/`405` — is `route="unmatched"`.
One JSON access-log line per request carries `method`, `route`, `status`, `duration_ms`
and `request_id`. Staff IDs, raw URLs and request IDs are never metric labels.

---

## Data

| Table | Holds |
|---|---|
| `config_namespace` | one row per namespace, with its `client`/`server` audience |
| `config_schema` | one row per `(namespace, schema_version)`; a schema is never edited in place |
| `config_draft` | the one working draft per namespace; `revision` is the optimistic-locking token |
| `config_version` | immutable snapshots, with their canonical `sha256` |
| `content_pack` | uploaded binaries, `sha256` UNIQUE because content is addressed by it |
| `release` | append-only manifest snapshots per channel |
| `channel_head` | the current release per channel — the row `SELECT … FOR UPDATE` locks to serialize publishes |
| `audit_log` | every mutating action, written in the same transaction as the change |

`migrations/00002_bootstrap_channels.sql` gives every channel a head before the first
publish, so Patch can answer for any channel from the moment the stack comes up rather
than `404`ing until somebody happens to publish. Each bootstrap release carries an empty
client manifest, written in the canonical form §3's hashing rule produces, so its
`manifest_sha256` is the same hash the publish path will compute for the same document.

Blob storage (CFG-A3) writes to a temp file and `rename`s it into
`blobs/ab/cd/<sha256>`, so uploading the same bytes twice stores one blob and a crash
mid-upload leaves no partial one. `SweepTemps` clears the temp files a crash did leave
behind, at startup, warning only — a failed sweep must not stop a service whose real work
does not depend on it.

### Seed namespaces

`internal/seed/seed/` holds one directory per starter namespace, and every file in it is
embedded into the binary with `//go:embed`. That is deliberate: the runtime image is
distroless and has no seed folder on disk to read, so `config seed` reads the tree from
memory. The `dockerfile` needs no extra `COPY` for it and `go build` carries it
automatically.

Each namespace directory has exactly three files:

| File | Holds |
|---|---|
| `namespace.json` | `{"audience":"client","description":"…"}` — the audience is checked against the API's `client`/`server` rule |
| `schema.json` | a JSON Schema draft 2020-12 document; the directory name is the namespace name and must match the API's slug regexp |
| `draft.json` | the initial working document, which must validate against `schema.json` |

`config seed` reads and checks the whole folder before touching Postgres, then, for each
namespace that does not exist, creates it (schema v1 and an empty draft), replaces the
schema with the seed schema (v2) and saves the seed draft. Every write goes through the
ordinary store API, so the audit rows name `seed`. Running it a second time is a no-op.

**Idempotency rule: an existing namespace is always skipped.** Seeding never overwrites a
human's schema or draft, whatever they now say. To add a namespace, create its directory
in `internal/seed/seed/` with the three files, make sure the name matches the slug rule
and the draft validates against the schema, and rebuild the image; `config seed
--dry-run` reports what the next real run would create.

---

## Tests

```sh
go test -race ./...
```

`internal/store` tests need a throwaway Postgres; without one they skip with
`CONFIG_TEST_DATABASE_URL not set`, so CI stays green without one. To run them:

```sh
docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=config_rw \
  -e POSTGRES_DB=config_test postgres:17
CONFIG_TEST_DATABASE_URL='postgres://config_rw:pw@localhost:5432/config_test' go test -race ./internal/store ./internal/seed
```

They create the schema themselves and leave rows behind; point them at a database you do
not care about. They check both of CFG-A2's claims — the second `goose.Up` is a no-op, and
`channel_head` holds exactly `dev`, `staging`, `live` — plus that the seeded manifest
hashes to what its own `manifest_sha256` column says (recomputing it the way the publish
path will: unmarshal, re-marshal, hash), and that an audit row written in a transaction
that rolls back leaves nothing behind (CFG-A4).

`internal/seed` has both halves: database-free tests that the embedded tree loads and
that a bad name, audience, schema or draft is rejected naming the file, and database
tests that the first `Apply` creates schema v2 and the seed draft, the second skips and
writes no audit rows, and a draft edited after seeding is never overwritten. They use
per-run namespace names so they can share the store tests' database.

`internal/server` runs the real `Run` on `:0` against an `httptest` JWKS and asserts the
CFG-A1 and CFG-B10 boundaries end to end: a missing, forged, expired or player-domain token
is `401` with the matching `code`; all twelve role/route combinations land on `403` or
through to `501` as the table says; and each rejection increments exactly one
`config_token_rejected_total{reason}` series.

The `internal/auth` tests stand up a JWKS serving an `OKP`/`Ed25519` JWK and check the
rejections one by one, including `alg: none` and the ordering of `ReasonFor` when a token
fails two ways at once (forged *and* stale reports `invalid_signature`) — the reason has to
be the one an operator should act on, not whichever `errors.Is` happened to match first.

### Concurrency guarantees and where they are tested

Every mutating store call takes a row lock and checks its optimistic guard inside one
transaction; each guard has a test that releases N goroutines from a start barrier and
asserts the exact split of winners and losers. The `channel_head` tests take a session
advisory lock (`lockChannel`/`lockDevHead`) and restore the head, because that row is
shared with the other package's test binary.

| Guarantee | Store tests | HTTP tests |
|---|---|---|
| One draft save wins a revision; the losers are stale, no update is lost | `TestSaveDraftIsOptimistic`, `TestConcurrentDraftSavesHaveOneWinner`, `TestChainedDraftSavesHaveNoLostUpdates` | `TestConcurrentDraftSavesThroughTheLiveServer` |
| One version is cut per revision, and it captures exactly the body that revision held | `TestCreateVersionIsSerialised`, `TestVersionWhileEditingRace` | `TestVersionsRoutesThroughTheLiveServer` |
| Schema replaces serialise into consecutive versions with no gaps | `TestReplaceSchemaIsSerialised` | `TestConcurrentSchemaReplacesThroughTheLiveServer` |
| One publish moves the head and notifies exactly once; the rest are `stale_release` | `TestPublishIsSerialised`, `TestPublishNotifiesOnlyAfterCommit` | `TestConcurrentPublishesThroughTheLiveServer`, `TestPublishConcurrentThroughTheLiveServer` |
| A rejected audit insert rolls the change back with it | `TestAuditLandsInTheCallersTransaction`, `TestAuditFailureRollsBackTheChange` | `TestAuditFailureIsA500ThroughTheLiveServer` |

All of these can run under `go test -race`; the barrier and the per-writer payloads are
what make a missing lock show up as a lost update rather than as a lucky interleaving.

### End-to-end smoke test

`smoke.sh` exercises the lifecycle against the built image: `migrate` twice with the
`release` count unchanged, the four boot refusals, `serve` with every endpoint, header and
metric checked, the guard against real signed staff tokens at each role and against a
forged one, `docker stop` for graceful shutdown, and a second container pointed at an
unreachable issuer to prove it still listens and still admits nobody.

It expects the image `otomo-config:skeleton`, a Postgres container named `otomo-pg` on a
Docker network named `otomo-test` (`config_rw`/`pw`, database `config`), `curl`, Docker
access, **and a Go toolchain**:

```sh
docker network create otomo-test
docker run -d --name otomo-pg --network otomo-test -e POSTGRES_USER=config_rw \
  -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=config postgres:17
docker build -t otomo-config:skeleton .
sudo sh smoke.sh
```

The Go toolchain is needed for `smoke/keygen`, which writes the smoke test's key material:
a JWKS that `nginx:alpine` then serves to the container, and one signed token per case. It
stands in for PHP Admin Auth, because this service deliberately has no key-material command
of its own — it verifies tokens and never mints them.

The same sequence minus Docker was run against the local Postgres and a local static JWKS
while the service was written, which is where the expected `code` of each rejection below
comes from. `smoke.sh` itself has not been executed here: the development machine has no
Docker inside WSL.

---

## Deploying

- The image runs as uid `65532` (distroless `nonroot`). The blob volume must be writable
  by that uid — note that an **empty named volume** arrives owned by root and fails the
  startup probe; set its ownership, or bind-mount a directory that already has it.
- Both ports are `EXPOSE`d; only the public one should ever be published.
- `CONFIG_STAFF_ISSUER` and `CONFIG_STAFF_AUDIENCE` must match what PHP Admin Auth puts in
  its tokens. A mismatch fails closed — every staff request 401s — but it wastes debugging
  time, so change them on both sides in the same deploy.
- The blob root is the volume Patch's nginx serves from, so Config must be the only writer
  and both containers must agree on the path.
- **Jenkins note:** `ci/services/Jenkinsfile` runs the container with no environment. With
  fail-fast config it will exit immediately on the missing `CONFIG_DATABASE_URL`. That is
  correct and intentional — wiring env and secrets into the deploy is COM-9, not this task.
  Do not add placeholder defaults to make the staging container stay up; it would come up
  "healthy" without a database or a blob volume and serve nothing.

---

## Decisions worth not re-litigating

1. **Module path is `github.com/otomo-live/otomo/services/config`**, not `config/config`.
2. **The token check is config-local (`internal/auth`), not the shared `platform` module.**
   This is COM-13 deferred, and the reason is mechanical: `ci/services/Jenkinsfile`
   sparse-checks out `services/${params.SERVICE}` alone, so an import of
   `services/platform` would not exist on CI. Extracting it needs a CI change first, not a
   code change. Auth has the same local copy for the same reason; when the extraction
   happens, both move at once.
3. **The guard is applied per route, not around the mux**, because the required role is a
   property of the route rather than of the service. The fallback is guarded too, so `404`
   and `405` are authenticated — an unauthenticated caller learns nothing about the table.
4. **`404` and `405` are answered by `api.NotFoundOrMethodNotAllowed`, not by ServeMux.**
   Go 1.22+'s mux answers a method mismatch with plain text and no COM-5 body. The handler
   resolves its answer by probing the mux with the path and a synthetic method, which is
   also what makes `Allow` correct for a wildcard path like
   `/namespaces/{ns}/draft`.
5. **No private key exists anywhere in this service.** Config verifies tokens against a
   JWKS and never mints one, so the staff signing key lives only inside PHP Admin Auth —
   the service cannot sign even if it is compromised. There is deliberately no config field
   for it.
6. **The JWKS client is built once and its own ticker is the retry.** `keyfunc` starts its
   refresh goroutine before the first fetch, so a client per retry would leave one
   goroutine behind per attempt. `Start` returns immediately and never blocks `serve`:
   readiness, not startup, is what reports that PHP Admin Auth has not answered yet.
   Likewise a *failed* refresh after a good one keeps the previous keys, so this service
   keeps verifying tokens while PHP Admin Auth is briefly down.
7. **Zero clock skew is accepted** (`CONFIG_JWT_CLOCK_SKEW=0s`) — see the table above.
8. **Blobs are a filesystem store behind an interface**, not S3 and not a table. It is
   addressed by hash, fanned out two levels, and written temp-then-rename.
9. **Two indexes exist that §3 does not list** (`release (channel, release_id DESC)` and
   `audit_log (at DESC)`). Both §5 history endpoints read newest-first and neither is
   served by a primary key alone. Recorded here rather than added silently.
10. **Store tests are gated on `CONFIG_TEST_DATABASE_URL`**, not testcontainers — a hard
    dependency on Docker would turn CI red for environmental reasons.
11. **Prometheus uses a private registry**, not the process-wide default one, so a second
    `Server` in a test cannot panic on duplicate registration.
12. **`/readyz` tolerates a nil dependency check** rather than panicking on one. A
    readiness endpoint that crashes is worse than useless: it cannot report unreadiness.

---

## Scope

**In:** config, both listeners and the middleware chain, the COM-5 error shape, metrics,
the §3 schema and its seeded channel heads (CFG-A2), blob storage (CFG-A3), the audit
writer (CFG-A4), staff-token verification and the §5 role table (CFG-A1, CFG-B10).

**Out (do not assume it exists):** every business handler in Phase B — namespace CRUD
(CFG-B1), schema upload (CFG-B2), draft save with optimistic locking (CFG-B3), validation
(CFG-B4), version creation and canonicalization (CFG-B5), diff (CFG-B6), pack upload
(CFG-B7), publish and `NOTIFY` (CFG-B8), history/rollback/promote (CFG-B9), manifest
signing (CFG-B12) — plus the Phase C seed namespaces, the WebUI, the Hurl contract tests
(CFG-E3), the Jenkins pipeline (CFG-E4/E5) and the shared `platform` module (COM-13).
