# Milestone 1 — Config

**Owner domain:** Staff (admin-auth tokens only)
**Prerequisites:** `00-common-stack.md` tasks COM-1 → COM-10 and WEB-1 → WEB-7
**Consumed by:** Patch (reads published releases), Dashboard (reads audit log)

---

## 1. Purpose and scope

Config is where staff author remote game configuration (balance values, feature flags, event schedules) and upload content packs, validate them, and publish them to a release channel without shipping a new client build.

**In scope for M1**

- Namespaced JSON configuration with JSON Schema validation
- Drafts with concurrent-edit protection
- Immutable versions, publish, rollback, diff
- Release channels: `dev`, `staging`, `live`
- Content pack (Godot `.pck`) upload with content-addressed storage
- Separation of client-visible and server-only configuration
- Audit log of every change

**Out of scope for M1**

- Scheduled publishes (publish at a future time)
- Per-player or percentage rollouts / A/B tests
- Approval workflows (two-person publish)
- Delta patching of content packs

### Core design rules

1. **Published data is immutable.** A version, once created, is never edited. A release is a pointer to a set of versions. Rollback moves the pointer.
2. **Everything published is content-addressed.** Each config document and content pack is stored under the SHA-256 hash of its bytes. Identical content is stored once, and caches never serve stale data because a changed file gets a new name.
3. **Audience is explicit.** Every namespace is marked `client` or `server`. Server-only config (for example matchmaking tuning next milestone) is never included in a client manifest.
4. **Config writes; Patch reads.** Config never serves game clients.

---

## 2. Tools you will need

| Tool | What it is | Role here |
|---|---|---|
| **PostgreSQL** `jsonb` column type | Binary JSON stored inside a relational table | Drafts, versions, schemas, releases, audit log |
| **JSON Schema** (draft 2020-12) | Standard language for describing the allowed shape of a JSON document (types, ranges, required fields) | Rejects bad config before publish. Example: `"damage": {"type":"number","minimum":0,"maximum":500}` |
| **`santhosh-tekuri/jsonschema`** | Go library that checks a document against a JSON Schema | Server-side validation (authoritative) |
| **`encoding/json/jsontext`** (Go std) | Low-level JSON tokens and values | Canonical serialization before hashing (see §3 note) |
| **@cfworker/json-schema** | JavaScript JSON Schema validator (draft 2020-12, no `eval`) | Instant client-side feedback in the editor (the server still re-validates). Replaced Ajv: Ajv compiles schemas with `new Function`, which the admin UI's CSP (`script-src 'self'`) blocks |
| **SHA-256** | Cryptographic hash function (standard library in every language) | Content addressing and integrity checks |
| **Blob storage** | Where binary files live. M1: a Docker named volume shared with Patch's nginx. Later: an S3-compatible object store (Garage, SeaweedFS or RustFS; avoid MinIO, whose community edition is no longer maintained) | Content packs and published JSON documents |
| **JSON Forms** | Generates editable forms from a JSON Schema; supports Angular, React and Vue | Friendly editor for simple namespaces |
| **CodeMirror 6** | Lightweight in-browser code editor | Raw JSON editing with syntax errors highlighted |
| **jsondiffpatch** | JSON-aware diff library with an HTML visual formatter | Showing what changed between versions |
| **Godot export (`--export-pack`)** | Godot CLI flag that exports only a `.pck` resource pack | Producing content packs to upload |

---

## 3. Data model

```sql
-- Schema definitions per namespace
config_namespace (
  name            text primary key,          -- e.g. 'balance.weapons'
  audience        text not null check (audience in ('client','server')),
  description     text,
  created_at      timestamptz not null default now()
)

config_schema (
  namespace       text references config_namespace(name),
  schema_version  int  not null,
  body            jsonb not null,             -- JSON Schema document
  created_by      text not null,              -- staff sub
  created_at      timestamptz not null default now(),
  primary key (namespace, schema_version)
)

-- One working draft per namespace
config_draft (
  namespace       text primary key references config_namespace(name),
  body            jsonb not null,
  base_version    int,                        -- version the draft was started from
  revision        int  not null default 1,    -- bumps on every save (optimistic locking)
  updated_by      text not null,
  updated_at      timestamptz not null default now()
)

-- Immutable snapshots
config_version (
  namespace       text references config_namespace(name),
  version         int  not null,
  schema_version  int  not null,
  body            jsonb not null,
  sha256          char(64) not null,          -- hash of canonical serialized bytes
  message         text not null,
  created_by      text not null,
  created_at      timestamptz not null default now(),
  primary key (namespace, version)
)

-- Uploaded binary content
content_pack (
  pack_id         uuid primary key,
  name            text not null,              -- e.g. 'season1_maps'
  sha256          char(64) not null unique,
  size_bytes      bigint not null,
  uploaded_by     text not null,
  uploaded_at     timestamptz not null default now()
)

-- A release = full manifest snapshot for a channel
release (
  release_id      bigserial primary key,
  channel         text not null check (channel in ('dev','staging','live')),
  manifest        jsonb not null,             -- see §4
  manifest_sha256 char(64) not null,
  server_manifest        jsonb,               -- §4; NULL only on rows from before migration 00003
  server_manifest_sha256 char(64),            -- NULL exactly when server_manifest is
  min_client_version text not null,
  message         text not null,
  created_by      text not null,
  created_at      timestamptz not null default now()
)

-- Current pointer per channel
channel_head (
  channel         text primary key,
  release_id      bigint not null references release(release_id),
  updated_by      text not null,
  updated_at      timestamptz not null default now()
)

audit_log (
  id              bigserial primary key,
  at              timestamptz not null default now(),
  actor_id        text not null,
  actor_name      text not null,              -- denormalized; staff DB is separate
  action          text not null,              -- 'draft.save','version.create','release.publish','release.rollback','pack.upload',...
  target          text not null,
  details         jsonb not null default '{}'
)
```

**Canonical serialization:** before hashing, serialize JSON with sorted keys and no insignificant whitespace. Otherwise the same logical document produces different hashes depending on key order. In Go, use `jsontext.Value`'s canonicalization method (RFC 8785 JSON Canonicalization Scheme) rather than writing your own; confirm the exact method name against the go1.27.1 stdlib docs (same API surface as 1.27.0 — the .1 patch touched `encoding/json` internals, not `jsontext`'s exported API, but verify before relying on it). Postgres `jsonb` does not preserve key order or formatting, so always canonicalize from the parsed value, never from what the database returns.

---

## 4. Manifest format

Built by Config at publish time, stored in `release.manifest`, served by Patch.

```json
{
  "format": 1,
  "channel": "live",
  "release_id": 42,
  "created_at": "2026-10-01T09:00:00Z",
  "min_client_version": "0.3.0",
  "config": {
    "balance.weapons": { "version": 7, "sha256": "ab12…", "size": 1834 },
    "features.flags":  { "version": 3, "sha256": "cd34…", "size": 211 }
  },
  "packs": [
    { "name": "season1_maps", "sha256": "ef56…", "size": 48211934 }
  ]
}
```

- Only `client`-audience namespaces appear in this manifest.
- Every release also stores a **server manifest** (design/13 CF-1): the same
  format, built by the same code with the same `release_id`, `created_at` and
  `min_client_version`, whose `config` holds only the `server`-audience namespaces the
  release selected, and whose `packs` is always `[]`. A publish lists client and server
  namespaces together in `versions`; Config sorts them by audience. Rollback moves the
  pointer, so both manifests move together; promote copies both. Release rows from
  before this existed have NULL `server_manifest`, which means an empty one. Patch serves
  it to Session and game servers on its internal listener only (CF-3); it never goes to
  clients.
- Clients fetch each item from `/patch/v1/blob/{sha256}`.
- Published config documents are written to blob storage under their hash at publish time, the same as packs.

---

## 5. API contract

All routes are under `/api/admin/config` and require a staff token.

| Method & path | Role | Purpose |
|---|---|---|
| `GET /namespaces` | viewer | List namespaces with audience, latest version, draft status |
| `POST /namespaces` | admin | Create namespace |
| `GET /namespaces/{ns}/schema` · `PUT` | viewer · admin | Read / replace schema (creates new `schema_version`) |
| `GET /namespaces/{ns}/draft` | viewer | Read draft with `revision` |
| `PUT /namespaces/{ns}/draft` | live_ops | Save draft; body includes `revision`; `409` on mismatch |
| `POST /namespaces/{ns}/draft/validate` | live_ops | Validate without saving; returns error list with JSON pointers |
| `POST /namespaces/{ns}/versions` | live_ops | Snapshot draft into new immutable version (requires message) |
| `GET /namespaces/{ns}/versions` · `/{v}` | viewer | Version history / one version |
| `GET /namespaces/{ns}/diff?from=&to=` | viewer | Diff between versions (or `to=draft`) |
| `POST /packs` | live_ops | Upload `.pck` (streamed); returns hash |
| `GET /packs` | viewer | List packs |
| `POST /channels/{ch}/releases` | live_ops (`dev`,`staging`) / admin (`live`) | Publish: choose versions + packs + `min_client_version` + message |
| `GET /channels/{ch}/releases` | viewer | Release history |
| `POST /channels/{ch}/rollback` | admin | Point channel head at an earlier `release_id` |
| `POST /channels/{ch}/promote?from=staging` | admin | Copy staging's current manifest to `live` as a new release |
| `GET /audit` | viewer | Paginated audit log (consumed by Dashboard) |

---

## 5a. Go implementation notes

- **Publish transaction:** use `pgx.BeginFunc` (or `pool.BeginTx` + deferred rollback). Take the channel lock with `SELECT … FROM channel_head WHERE channel = $1 FOR UPDATE`. Send `NOTIFY` with `SELECT pg_notify('config_release', $1)` inside the transaction; Postgres only delivers it after commit, which is exactly the ordering you need.
- **Blob writes before commit:** write blobs to storage before inserting the release row. If the transaction then fails, an orphaned blob is harmless; a release pointing at a missing blob is not.
- **Pack upload streaming:** `io.Copy(io.MultiWriter(tmpFile, sha256Hasher), http.MaxBytesReader(w, r.Body, maxPackBytes))`, then `tmpFile.Sync()`, then `os.Rename` into the fan-out path. Check the first 4 bytes for `GDPC` with a small `bufio.Reader.Peek(4)` before copying.
- **Schema compilation cache:** compile each JSON Schema once per `(namespace, schema_version)` and keep the compiled validator in a map guarded by `sync.RWMutex`; compiling on every validation request is wasted work.
- **Strict JSON input:** the Go 1.27 JSON engine rejects duplicate keys and invalid UTF-8 by default. Keep those defaults for draft bodies; a config file with two `"damage"` keys should be an error, not a silent last-one-wins.
- **Optimistic locking:** check `CommandTag.RowsAffected() == 0` from the `UPDATE … WHERE revision = $2` to return `409`.

## 6. Task breakdown

### Phase A — Service foundation

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-A1 | Create service from template; Gateway route `/api/admin/config/*` with staff issuer; in-service token and role re-check | Player token and missing token both 401 | COM-2, COM-4 |
| CFG-A2 | Write migrations for all §3 tables; seed `channel_head` rows with an empty release per channel | Migration job runs in Jenkins before deploy; re-running is a no-op | COM-6 |
| CFG-A3 | Blob storage abstraction: `put(bytes or stream) → sha256`, `exists(sha256)`, `open(sha256)`. M1 implementation writes to the shared volume as `blobs/ab/cd/abcd…` (two-level fan-out avoids huge directories); write to a temp file then atomic rename | Uploading the same file twice stores it once; a crash mid-upload leaves no partial blob | CFG-A1 |
| CFG-A4 | Audit writer: every mutating handler writes its audit entry in the same database transaction as the change | Forcing a failure after the change but before commit leaves neither change nor audit row | CFG-A2 |

### Phase B — Backend features

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-B1 | Namespace CRUD with audience flag | Audience cannot be changed after the first version exists | CFG-A2 |
| CFG-B2 | Schema upload: validate that the upload is itself a valid JSON Schema; store as new `schema_version` | Invalid schema rejected with 400 | CFG-B1 |
| CFG-B3 | Draft save with optimistic locking: `UPDATE … WHERE namespace = $1 AND revision = $2`; zero rows updated → `409` with current draft | Two concurrent saves: one succeeds, one gets 409 | CFG-A2 |
| CFG-B4 | Validation endpoint: returns errors as `[{pointer: "/weapons/3/damage", message: "…"}]` | Errors map to exact fields in the UI | CFG-B2 |
| CFG-B5 | Create version: re-validate draft against current schema, canonicalize, hash, insert immutable row | Invalid draft cannot become a version | CFG-B4 |
| CFG-B6 | Diff endpoint: structural JSON diff between two versions or version-vs-draft | Array reorder and nested value changes shown correctly | CFG-B5 |
| CFG-B7 | Pack upload: stream request body to blob storage while hashing (never load whole file in memory); enforce max size (e.g. 512 MB); verify file starts with Godot PCK magic bytes `GDPC` | 400 MB upload keeps service memory flat; non-PCK file rejected | CFG-A3 |
| CFG-B8 | Publish: in one transaction — lock `channel_head` row (`SELECT … FOR UPDATE`), resolve selected versions, write each config body to blob storage, build client manifest, canonicalize + hash, insert `release`, move head, write audit, then `NOTIFY config_release, '<channel>'` | Two simultaneous publishes to the same channel are serialized; Patch sees the notification | CFG-B5, CFG-B7, CFG-A4 |
| CFG-B9 | Release history, rollback, promote-staging-to-live, audit endpoint | Rollback produces a notification and Patch serves the older manifest | CFG-B8 |
| CFG-B10 | Role enforcement per §5 table, including `live` publish requiring `admin` | `live_ops` publishing to `live` returns 403 | CFG-A1 |
| CFG-B11 | Metrics: `config_publish_total{channel}`, `config_validation_failures_total`, `config_pack_upload_bytes_total` | Visible on Dashboard | DSH-B1 |
| CFG-B12 | (Stretch) Manifest signing: sign `manifest_sha256` with a private key mounted only into Config; store signature on the release. Godot's `Crypto.verify` works with RSA keys, so use RSA and prototype client verification before committing | Tampered manifest fails client verification | CFG-B8 |

### Phase C — Seed content for the demo

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-C1 | Define 2–3 real namespaces with schemas the Godot client will actually read (e.g. `features.flags`, `balance.player`, `ui.motd` for a message of the day) | Schemas committed in a repo folder and loaded by a seed script | CFG-B2 |
| CFG-C2 | Godot content pack export: a small `.pck` containing a texture or scene, exported with `--export-pack` in a Jenkins job or by hand | Pack loads in the client when mounted manually | — |

### Phase D — Frontend module (inside the admin WebUI)

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-D1 | Namespace list: audience badge, latest version, "draft has unpublished changes" indicator | Matches API state | CFG-B1, WEB-5 |
| CFG-D2 | Schema-driven form editor using JSON Forms for namespaces whose schemas are simple; time-box custom renderers to 2 days | Editing a number respects min/max from schema | CFG-B4 |
| CFG-D3 | Raw JSON editor (CodeMirror 6) with local JSON Schema validation (`@cfworker/json-schema`) on each keystroke (debounced) and server validation on save; error markers at JSON pointers | Invalid value highlighted before saving | CFG-B4 |
| CFG-D4 | Save handling: send `revision`; on `409`, show a dialog with the other editor's changes and let the user reload or copy their own edits out | No silent overwrite of another staff member's work | CFG-B3 |
| CFG-D5 | Diff view (jsondiffpatch visual formatter) used in version history and before publishing | Shows added/removed/changed values clearly | CFG-B6 |
| CFG-D6 | Create-version dialog requiring a message | Empty message blocked | CFG-B5 |
| CFG-D7 | Pack upload page with progress bar, displays resulting hash and size | 100 MB upload shows progress and completes | CFG-B7 |
| CFG-D8 | Release composer per channel: pick version per namespace (defaults to latest), pick packs, set `min_client_version`, preview manifest diff against current head, confirm with message; `live` requires typing the channel name to confirm | Publish to live requires deliberate confirmation | CFG-B8 |
| CFG-D9 | Release history with rollback and promote buttons (admin only) | Rollback updates the view immediately | CFG-B9 |

### Phase E — Testing and delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CFG-E1 | Unit tests: canonical serialization is stable across key orders; manifest builder excludes `server` namespaces | Pass in Jenkins | CFG-B8 |
| CFG-E2 | Integration tests (real Postgres via Testcontainers or compose): concurrent draft saves, concurrent publishes, rollback, audit row in same transaction | Pass in Jenkins | Phase B |
| CFG-E3 | Hurl contract tests covering every role boundary in §5 | Pass in Jenkins | CFG-B10 |
| CFG-E4 | Jenkins pipeline (COM-8) with the migration step before deploy | Push to main deploys | COM-8 |
| CFG-E5 | Backup: nightly `pg_dump` of the `config` database plus a copy of the blob volume to a second location | Restore rehearsed once on a scratch VM | CFG-A2 |

---

## 7. Definition of done

- Staff can create a namespace, edit it with validation, version it, publish it to `dev`, promote to `live`, and roll back.
- A server-only namespace never appears in any client manifest.
- A content pack uploaded through the UI appears in a published manifest with the correct hash.
- Every change is visible in the Dashboard audit trail with the staff member's name.
- Patch serves the new manifest within 5 seconds of publish.

## 8. Risks

| Risk | Mitigation |
|---|---|
| A bad value reaches live and breaks clients | Schema validation on server, staging channel, diff preview, one-click rollback |
| Two staff members overwrite each other's edits | Optimistic locking (CFG-B3, CFG-D4) |
| Secrets or server-only tuning leak to clients | Audience flag enforced in manifest builder with a unit test (CFG-E1) |
| Blob volume and database drift apart | Blobs written before the release row commits; blobs are never deleted in M1 (garbage collection later) |
| Schema-form generator consumes the schedule | Time-box CFG-D2; raw JSON editor covers everything |
