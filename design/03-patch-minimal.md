# Milestone 1 — Patch (minimal)

**Scope:** all of Patch: Phase A (blob delivery), Phase B (manifest service), and the server manifest (CF-3 in `13-player-plane-plan.md`)
**Owner domain:** Player / public
**Prerequisites:** `00-common-stack.md` COM-1 → COM-10; Config tasks CFG-A3 and CFG-B8
**Consumers:** Godot client (M1), Godot headless servers (next milestone)

---

> **State (2026-09-28):** Patch is deployed and public at
> `https://play.example.com/patch/v1/…`. Blobs and manifests are served by the Go service
> itself (`services/patch/internal/api/blob.go`, `manifest.go`), not by the nginx location
> the Phase A table describes. Each Phase A and B acceptance criterion was checked against
> the code on 2026-09-29 and holds; PAT-A2 and PAT-A5 now describe what the Go service does.
> The checks that need the running service (a 400 MB download through the gateway, `Range`
> resume, a publish served within 5 s, a killed listener) are still to be run. Behind the edge,
> cookies are dropped and the client IP arrives via the gateway's trusted-proxy handling
> (`15-allocator-and-edge-plan.md` §3).

## 1. Purpose and scope

Patch is a small, read-only, heavily cached service that tells game clients what the current release is and delivers the files that release refers to. It holds no authoring logic; that lives in Config.

**In scope for M1**

- Current manifest per channel with HTTP caching (`ETag` / `304 Not Modified`)
- Content-addressed blob delivery (config JSON and `.pck` files)
- Resumable downloads
- Channel access control (`live` public, `dev`/`staging` restricted)
- Godot client updater: check, download, verify, cache, mount, offline fallback

**Out of scope for M1**

- Delta/binary patches between pack versions
- CDN integration
- Per-player targeting
- Full client executable updates (handled by the store or launcher)

### Why it is small

The manifest is the only thing that changes. Everything else is addressed by hash and never changes, so it can be cached forever. Once a client is up to date, a check costs one request returning `304` with no body.

---

## 2. Tools you will need

| Tool | What it is | Role here |
|---|---|---|
| **Go** (see common doc) | — | Manifest endpoint, channel access control, change notifications |
| **`pgx/v5` `Conn.WaitForNotification`** | pgx method that blocks until a `NOTIFY` arrives on a dedicated connection | Receiving publish/rollback notifications |
| **PostgreSQL `LISTEN/NOTIFY`** | Built-in Postgres publish/subscribe: a connection runs `LISTEN config_release` and receives messages sent with `NOTIFY` | Patch learns immediately when Config publishes or rolls back |
| **nginx** | HTTP server with zero-copy file sending (`sendfile`), built-in `Range` request support | Serves blob files straight from the shared volume; Patch service never copies large files through its own process |
| **nginx `auth_request` / `X-Accel-Redirect`** | nginx features that let an application decide access and then hand the file transfer back to nginx | Only needed if blobs for restricted channels must be access-controlled (see PAT-B6) |
| **HTTP caching headers** | `ETag`, `If-None-Match`, `Cache-Control` | Cheap update checks and immutable blobs |
| **Godot `HTTPRequest` node** | Godot's high-level HTTP client; supports custom headers and writing responses directly to a file (`download_file`) | Manifest checks and blob downloads |
| **Godot `HashingContext`** | Incremental hashing (feed data in chunks) | Verifying SHA-256 of large packs without loading them fully into memory |
| **Godot `ProjectSettings.load_resource_pack()`** | Mounts a `.pck` into Godot's virtual `res://` filesystem at runtime | Activating downloaded content |
| **Godot `FileAccess` / `DirAccess`** | File and directory APIs | Cache management and atomic file replacement under `user://` |

---

## 3. Architecture

```
Config ──(publish tx)──► Postgres: release, channel_head ──NOTIFY config_release──┐
   │                                                                              ▼
   └──writes blobs──► shared volume: blobs/ab/cd/<sha256>                  Patch service
                               ▲                                   (in-memory current manifest
                               │ sendfile                            per channel + ETag)
                          nginx (blobs)                                          ▲
                               ▲                                                 │
Godot client ──► Gateway ──────┴── /patch/v1/blob/{sha256}                       │
                         └──────── /patch/v1/{channel}/manifest ─────────────────┘
```

- Patch connects to Postgres as `patch_ro` (read-only on the `config` database).
- Patch keeps each channel's current manifest bytes and hash in memory; manifest requests never hit the database.
- `NOTIFY` messages are lost if the listening connection drops, so Patch also re-reads channel heads every 60 seconds and immediately after reconnecting.

---

## 4. API contract

| Method & path | Access | Behavior |
|---|---|---|
| `GET /patch/v1/{channel}/manifest` | `live`: public. `dev`/`staging`: staff token | Returns manifest JSON. `ETag: "<manifest_sha256>"`, `Cache-Control: no-cache` (client must revalidate). Request with matching `If-None-Match` returns `304` with empty body |
| `GET /patch/v1/blob/{sha256}` | See PAT-B6 | Served by nginx. `Cache-Control: public, max-age=31536000, immutable`. Supports `Range` for resume. `404` if unknown |
| `GET /patch/v1/{channel}/manifest/signature` | Same as manifest | (Stretch, with CFG-B12) Detached signature |

Response headers on manifest also include `X-Min-Client-Version` so a client can decide to stop before parsing.

**Server manifest (CF-3).** Served on a separate internal listener
(`PATCH_INTERNAL_ADDR`, `:8081`) that no gateway routes to, never on the public one:

| Method & path | Access | Behavior |
|---|---|---|
| `GET /internal/patch/server-manifest/{channel}` | D4 service key `patch_session.key` (Session). Missing or wrong key: `401`; another role's key: `403` | The channel's current release's server manifest (`release.server_manifest`), hash-checked at load like the client manifest. `ETag`, `Cache-Control: no-cache`, `304` on a matching `If-None-Match`, and `X-Otomo-Release: <release_id>`. A NULL `server_manifest` serves an empty manifest (`"config":{}`, `"packs":[]`), not an error |
| `GET /internal/patch/blob/{sha256}` | Same key | The same blob handler as the public route |

**Validation:** `{sha256}` must match `^[0-9a-f]{64}$` before touching the filesystem (prevents path traversal). `{channel}` must be one of the known channels.

---

## 4a. Go implementation notes

- **In-memory manifests:** one `atomic.Pointer[manifestSet]` holding, per channel, the pre-serialized manifest `[]byte`, its quoted ETag string and `min_client_version`. Reloads build a new set and swap the pointer. Handlers do one pointer load, one header compare, and one `w.Write`; no locks, no JSON encoding per request.
- **Listener:** acquire a dedicated connection from the pool (`pool.Acquire`, then `conn.Hijack()` so it isn't returned), run `LISTEN config_release`, then loop on `WaitForNotification(ctx)`. On error: log, back off, reconnect, reload all channels (PAT-B4).
- **Fallback poll:** a `time.Ticker` goroutine comparing each channel's `release_id` with the loaded one; reload only on change.
- **ETag compare:** `If-None-Match` can contain multiple comma-separated tags or `*`; handle both, not just exact string equality.
- **Readiness:** `/readyz` returns 503 until the first full load succeeds and while the listener has been disconnected for longer than the poll interval.

## 5. Task breakdown

### Phase A — Blob delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| PAT-A1 | Mount the blob volume read-only into an nginx container | Patch nginx cannot write to the volume | CFG-A3 |
| PAT-A2 | nginx location for `/patch/v1/blob/`: regex-validate the hash, map to `blobs/ab/cd/<hash>`, `sendfile on`, immutable cache headers, `Content-Type: application/octet-stream` | Valid hash downloads; `../` or malformed hash returns 400 `validation_failed` without filesystem access (an unknown hash is 404) | PAT-A1 |
| PAT-A3 | Confirm `Range` requests work (`curl -r 0-1023`) and return `206 Partial Content` | Interrupted download resumes | PAT-A2 |
| PAT-A4 | Gateway route `/patch/v1/blob/*` → Patch nginx, with response buffering off for large files and a sensible per-IP rate limit | 400 MB file downloads through Gateway without Gateway memory growth | PAT-A2 |
| PAT-A5 | Access log in JSON format so Alloy ships it to Loki. Blob requests are in Patch's own access log, so the label is `service="patch"` (route `/patch/v1/blob/{sha256}`) | Blob downloads visible in Dashboard logs | DSH-A3 |

### Phase B — Manifest service

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| PAT-B1 | Create service from template; Gateway route `/patch/v1/*/manifest` | `/readyz` fails until manifests are loaded | COM-2 |
| PAT-B2 | On startup: load `channel_head` joined to `release` for all channels into memory (manifest bytes, hash, `min_client_version`) | Correct manifests served immediately after start | CFG-B8 |
| PAT-B3 | Dedicated Postgres connection running `LISTEN config_release`; on message, reload that channel and atomically swap the in-memory entry | New manifest served within 5 s of publish; no request ever sees a half-updated entry | PAT-B2 |
| PAT-B4 | Fallback: poll channel heads every 60 s; on listener reconnect, reload all channels | Killing the listener connection does not leave stale manifests for more than 60 s | PAT-B3 |
| PAT-B5 | Manifest endpoint with `ETag` / `If-None-Match` → `304` handling, `X-Min-Client-Version` header | curl with the current ETag gets 304 and zero body bytes | PAT-B2 |
| PAT-B6 | Channel access: `live` public; `dev`/`staging` require staff token (staff issuer on that specific route). M1 accepts that blob hashes of restricted channels are unguessable but not access-controlled; document it. If that is not acceptable, add `auth_request` to nginx calling a Patch endpoint that checks whether the hash belongs to a channel the caller may see | Player/no token on `staging` manifest returns 401; decision documented | PAT-B5, COM-4 |
| PAT-B7 | Metrics: `patch_manifest_requests_total{channel,result="200|304"}`, `patch_manifest_reload_total{source="notify|poll"}`, `patch_current_release_id{channel}` gauge | Dashboard shows 304 ratio and current release per channel | DSH-B1 |

### Phase C — Godot client updater

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| PAT-C1 | `PatchClient` autoload singleton with states: `CHECKING → DOWNLOADING → VERIFYING → MOUNTING → READY`, plus `OFFLINE_READY` and `CLIENT_TOO_OLD`; emits signals for UI | States visible in a debug overlay | — |
| PAT-C2 | Local cache layout under `user://patch/`: `manifest.json`, `etag.txt`, `blobs/<sha256>`; store last good manifest only after all its files verify | Deleting a blob file triggers re-download on next launch | PAT-C1 |
| PAT-C3 | Manifest check: send `If-None-Match` from `etag.txt`; `304` → use cached manifest; `200` → parse, compare `min_client_version` with the client's build version (semantic version compare) → `CLIENT_TOO_OLD` if lower | Update check with no changes transfers only headers | PAT-B5 |
| PAT-C4 | Download each missing blob with `HTTPRequest.download_file` to `<sha256>.part`; resume using a `Range` header if a `.part` exists; limit to 2 concurrent downloads | Killing the game mid-download and relaunching resumes instead of restarting | PAT-A3 |
| PAT-C5 | Verify: hash the `.part` file in 1 MB chunks with `HashingContext` (SHA-256); on match, rename to final name; on mismatch, delete and retry once, then fail | Corrupting a byte in the download causes rejection | PAT-C4 |
| PAT-C6 | Config consumption: load verified config JSON blobs into a `RemoteConfig` autoload with typed getters and compiled-in defaults for every key | Game runs with defaults if a key is missing | PAT-C5 |
| PAT-C7 | Pack mounting: call `ProjectSettings.load_resource_pack()` for each verified pack **before** loading any scene that uses its resources; decide and document whether packs may override base game files | Content from the pack appears in-game | PAT-C5, CFG-C2 |
| PAT-C8 | Offline boot: if Patch is unreachable, use the last good cached manifest (`OFFLINE_READY`); if no cache exists, use compiled-in defaults and no packs | Game starts with Gateway stopped | PAT-C2 |
| PAT-C9 | Periodic re-check (e.g. every 5 min while in menus) for config-only changes; apply hot-safe config immediately, defer pack changes to next launch or return to main menu | Changing `ui.motd` in Config updates the menu without restart | PAT-C3 |
| PAT-C10 | Cache cleanup: after a successful update, delete blobs not referenced by the current manifest | `user://patch/blobs` does not grow unbounded | PAT-C5 |
| PAT-C11 | Security note in code: only mount packs whose hashes came from a manifest fetched over TLS (and, with CFG-B12, whose signature verified). Packs can contain scripts, so an unverified pack is arbitrary code execution | Reviewed and documented | PAT-C7 |

### Phase D — Testing and delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| PAT-D1 | Hurl tests: 200 then 304 with ETag; malformed hash 404; staging without staff token 401 | Pass in Jenkins | Phase A, B |
| PAT-D2 | Integration test: publish in Config → manifest changes within 5 s; rollback → previous manifest returns | Pass in Jenkins compose stack | PAT-B3, CFG-B9 |
| PAT-D3 | k6 test: 2,000 simulated clients polling the manifest every 60 s with ETag | p95 < 20 ms; Patch CPU minimal | PAT-B5 |
| PAT-D4 | Manual Godot test matrix: fresh install, up-to-date, update available, interrupted download, corrupted blob, offline with cache, offline without cache, client too old | All eight cases behave as specified | Phase C |
| PAT-D5 | Jenkins pipelines for Patch service and Patch nginx image (COM-8) | Push to main deploys | COM-8 |

---

## 6. Definition of done

- Publishing in Config changes what a running Godot client sees without restarting (config) or on next launch (packs).
- An up-to-date client's check costs one `304` response.
- Interrupted and corrupted downloads recover automatically.
- The game starts with the backend offline.

## 7. Risks

| Risk | Mitigation |
|---|---|
| Missed `NOTIFY` leaves stale manifests | 60 s poll fallback and reload on reconnect (PAT-B4) |
| Malicious or corrupted pack executes code in clients | Hash verification, TLS, optional signing (PAT-C11, CFG-B12) |
| Large downloads through Gateway exhaust memory | Buffering disabled; nginx `sendfile` (PAT-A4) |
| Restricted-channel blobs downloadable by hash | Documented M1 trade-off with an upgrade path (PAT-B6) |
