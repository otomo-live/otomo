# Patch

Read-only release-manifest and blob service. Tells the Godot client what the current
release is for a channel and points it at the content-addressed blobs that release
names. It holds no authoring logic — that lives in Config — and it reads Config's
Postgres database as the read-only role `patch_ro`, so a compromised Patch container
cannot change a release.

Source of truth: `design/03-patch-minimal.md` (§3 architecture, §4 API, §4a Go notes) and
`design/00-common-stack.md` (COM-1…COM-14).

---

## What exists today

PAT-B1 through PAT-B5 and PAT-A2′:

- One command, `serve` (the default). No `migrate`: Patch owns no schema.
- Both HTTP listeners, structured JSON logging, Prometheus metrics (`patch_` prefix),
  request IDs, panic recovery and graceful shutdown.
- A read-only Postgres pool (`patch_ro`) with a cached readiness ping; `/readyz` is 503
  until the first full manifest load.
- The manifest set: `channel_head` joined to `release`, loaded into an immutable,
  lock-free in-memory set with the canonical body re-derived and hash-checked
  (`patch_current_release_id{channel}` gauge).
- Reload on `LISTEN config_release` with a fallback poll (`PATCH_POLL_INTERVAL`,
  default 60 s) and a full reload after a reconnect.
- `GET /patch/v1/{channel}/manifest`: ETag, `If-None-Match` (list and `*`), 304 with
  no body, `X-Min-Client-Version`; `dev` and `staging` require a staff token verified
  in the service, `live` is public.
- `GET /patch/v1/blob/{sha256}`: `http.ServeContent` over the read-only blob mount,
  the hash checked before any filesystem access, immutable cache headers, `Range`.
  The player gateway exposes it at `/patch/v1/blob/` as a `Stream` route.
- The server manifest (CF-3) on a third listener, `PATCH_INTERNAL_ADDR`
  (`:8081`), that no gateway routes to and no port publishes. Every route on it needs
  the D4 key Session presents (`Authorization: Bearer <patch_session.key>`); a missing,
  malformed or unknown key is `401`, another role's key `403`:
  - `GET /internal/patch/server-manifest/{channel}`: the channel's current server
    manifest, loaded and hash-checked from the same `release` row as the client
    manifest (a row where either fails keeps the channel's last good release). ETag,
    `If-None-Match` and 304 as for the client manifest, plus `X-Otomo-Release` with the
    release id. A release whose `server_manifest` is NULL serves an empty manifest: the
    client manifest's fields with `"config":{}` and `"packs":[]`, which is what Config
    writes for a release with no server namespaces.
  - `GET /internal/patch/blob/{sha256}`: the same blob handler as the public route.


---

## Running it

```sh
export PATCH_DATABASE_URL='postgres://patch_ro:pw@localhost:5432/config'
export PATCH_BLOB_ROOT=./.blobs
export PATCH_STAFF_JWKS_URL=http://localhost:8081/.well-known/jwks.json

go run . serve     # or just: go run .
```

`serve` refuses to start if Postgres is unreachable, the blob root is not a directory,
or the configuration is invalid. An unavailable JWKS does **not** stop start-up; the
service reports itself unready until a key arrives, which is what keeps a Patch deploy
from being coupled to an Admin Auth deploy.

### Tests

```sh
go test -count=1 ./...
PATCH_TEST_DATABASE_URL='postgres://patch_ro:pw@localhost:5432/config_test' go test -count=1 ./...
```

The store and manifest tests skip unless `PATCH_TEST_DATABASE_URL` is set, and the
manifest database test skips too when Config's migrations have not been applied, so CI
stays green without a database.

---

## Configuration

Every variable is read once at start-up. `PATCH_DATABASE_URL`, `PATCH_BLOB_ROOT` and
`PATCH_STAFF_JWKS_URL` are required; the rest have defaults. A configuration error
prints **every** problem at once and exits `1`.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `PATCH_LISTEN_ADDR` | no | `:8080` | Public listener. Gateway proxies `/patch/v1/*` here. |
| `PATCH_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `PATCH_INTERNAL_ADDR` | no | `:8081` | Internal callers only: the server manifest and its blobs (CF-3). No gateway routes here. |
| `PATCH_SESSION_KEY_PATH` | no | none | Session's D4 key file (`deploy/secrets/service_keys/patch_session.key`). Unset: every internal route answers `401`. Set but unreadable, not base64url or under 32 bytes: start-up fails. |
| `PATCH_DATABASE_URL` | **yes** | — | `postgres://patch_ro:...@postgres:5432/config`. Read-only. |
| `PATCH_DB_MAX_CONNS` | no | `4` | See the connection budget. |
| `PATCH_BLOB_ROOT` | **yes** | — | Config's blob volume, mounted read-only. Must be a directory. |
| `PATCH_STAFF_JWKS_URL` | **yes** | — | PHP Admin Auth's JWKS. Validated as an absolute URL at boot. |
| `PATCH_STAFF_ISSUER` | no | `https://admin-auth.otomo.internal` | Checked against the token's own `iss`. |
| `PATCH_STAFF_AUDIENCE` | no | `otomo:staff` | Checked against the token's own `aud`. |
| `PATCH_STAFF_JWKS_REFRESH` | no | `30s` | How often the key set is re-fetched. |
| `PATCH_JWT_CLOCK_SKEW` | no | `30s` | Leeway on `exp`/`nbf`. **`0` is accepted.** |
| `PATCH_POLL_INTERVAL` | no | `60s` | Fallback manifest poll (PAT-B4). |
| `PATCH_READ_TIMEOUT` | no | `10s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `PATCH_WRITE_TIMEOUT` | no | `30s` | |
| `PATCH_IDLE_TIMEOUT` | no | `120s` | |
| `PATCH_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by all three listeners. |
| `PATCH_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `PATCH_TEST_DATABASE_URL` | no | — | Read by the `internal/store` and `internal/manifest` tests. Never set in a deployment. |

Keep the sum of every service's `PATCH_DB_MAX_CONNS` / `CONFIG_DB_MAX_CONNS` below
Postgres `max_connections` (COM-14).

### Readiness

`/readyz` is 200 only when **all** of these hold:

1. `store.DB.Ready` — Postgres reachable (ping cached for 10 s).
2. `blob.Root.Ready` — `PATCH_BLOB_ROOT` is still a directory.
3. `auth.Verifier.Ready` — Admin Auth's JWKS has at least one key.
4. `Deps.Manifests` — at least one manifest Set has been loaded.

The first three are joined and reported together. The fourth is
`manifest.Holder.Ready`, which returns `manifests not loaded` until the first
`LoadAll` publishes a set. A Config database with no channel heads yet therefore
leaves a fresh Patch at 503 without crash-looping it; PAT-B4's poll retries until
it succeeds.
