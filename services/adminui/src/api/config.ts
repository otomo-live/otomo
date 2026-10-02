import { request, uploadWithProgress, type UploadProgress } from '@/api/client'
import { ApiError } from '@/api/errors'
import { arrayIn, bool, isRecord, nullableStr, num, objectIn, recordsIn, str } from '@/api/shape'

/**
 * The Config client: the namespaces, their schema, and the draft in flight.
 *
 * The paths are design/02-config.md section 5, prefix and all. gateway_dev forwards
 * `/api/admin/config/...` unstripped, so nothing here is relative to a base URL.
 *
 * The versions and the packs, channels and releases set are ABSENT rather than
 * forgotten. Both belong to the version and release composer, which is CFG-D* and
 * out of scope for this shell. A function with no caller is a function nothing
 * keeps honest: it compiles, it is never run, and the day it is needed it is the
 * one part of the module that has never been exercised. They go in when a view
 * needs them, with that view's tests. Namespace create and schema replace DO have
 * callers (the list's create dialog and the schema page), so they are here.
 *
 * Every response is read rather than cast, and a body that is not the documented
 * shape is an ApiError rather than a page full of `undefined`. See api/shape.ts
 * for why that is worth the lines.
 */

const BASE = '/api/admin/config'

export interface NamespaceSummary {
  name: string
  /** `client` or `server` (design/02-config.md rule 3). */
  audience: string
  description: string
  latestVersion: number
  /** The draft in flight: its revision, when it last moved, and whether it is ahead of the last version. */
  draft: {
    revision: number
    updatedAt: string
    hasUnpublishedChanges: boolean
  }
}

export interface SchemaSnapshot {
  namespace: string
  schemaVersion: number
  /** A JSON Schema document, draft 2020-12. Untyped on purpose: it is data. */
  schema: unknown
  createdBy: string
  createdAt: string
}

export interface DraftSnapshot {
  namespace: string
  /** The document itself. Untyped on purpose: it is whatever the namespace's schema describes. */
  document: unknown
  revision: number
  /**
   * The version this draft was last taken from. What a future version composer
   * needs, and what tells a reader whether the draft has ever been published.
   */
  baseVersion: number
  updatedBy: string
  updatedAt: string
}

export interface SavedDraft {
  revision: number
  updatedAt: string
}

export interface ValidationIssue {
  /** A JSON Pointer into the document, `''` for the document itself. */
  pointer: string
  message: string
}

export interface ValidationResult {
  valid: boolean
  errors: ValidationIssue[]
}

function readNamespace(row: Record<string, unknown>): NamespaceSummary {
  const draft = isRecord(row.draft) ? row.draft : {}
  return {
    name: str(row.name),
    audience: str(row.audience),
    description: str(row.description),
    latestVersion: num(row.latest_version),
    draft: {
      revision: num(draft.revision),
      updatedAt: str(draft.updated_at),
      hasUnpublishedChanges: bool(draft.has_unpublished_changes),
    },
  }
}

export async function listNamespaces(): Promise<NamespaceSummary[]> {
  const path = `${BASE}/namespaces`
  return recordsIn(await request<unknown>(path), 'namespaces', path).map(readNamespace)
}

/** The POST body. Audience cannot be changed after creation (design/02-config.md rule 3). */
export interface CreateNamespaceInput {
  name: string
  audience: 'client' | 'server'
  description: string
}

/**
 * Creates a namespace and returns it in the same shape the list uses.
 *
 * A duplicate name is a `409 already_exists` and a bad name is a
 * `400 validation_failed`; both arrive as a thrown ApiError, and the create
 * dialog branches on the code to place the message on the field rather than
 * closing the form.
 */
export async function createNamespace(input: CreateNamespaceInput): Promise<NamespaceSummary> {
  const path = `${BASE}/namespaces`
  return readNamespace(
    objectIn(await request<unknown>(path, { method: 'POST', body: input }), path),
  )
}

export async function getSchema(namespace: string): Promise<SchemaSnapshot> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/schema`
  const body = objectIn(await request<unknown>(path), path)
  return {
    namespace: str(body.namespace, namespace),
    schemaVersion: num(body.schema_version),
    schema: body.schema ?? null,
    createdBy: str(body.created_by),
    createdAt: str(body.created_at),
  }
}

/**
 * Replaces a namespace's schema, returning the new version.
 *
 * The request body IS the schema document, not an object wrapping it: Config's
 * `replaceSchema` compiles the body directly (services/config/internal/api/schemas.go).
 * The new version is immutable, which is why the whole snapshot comes back and
 * the caller adopts its `schemaVersion` rather than incrementing its own.
 */
export async function putSchema(
  namespace: string,
  schema: unknown,
  expectedVersion: number,
): Promise<SchemaSnapshot> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/schema`
  // The version this edit started from: Config refuses the write with
  // 409 stale_schema if someone saved a newer one, and with 428 if it is missing.
  const body = objectIn(
    await request<unknown>(path, {
      method: 'PUT',
      body: schema,
      headers: { 'If-Match': `"${expectedVersion}"` },
    }),
    path,
  )
  return {
    namespace: str(body.namespace, namespace),
    schemaVersion: num(body.schema_version),
    schema: body.schema ?? null,
    createdBy: str(body.created_by),
    createdAt: str(body.created_at),
  }
}

/** Someone saved a newer schema since the one the edit started from. */
export function isStaleSchema(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'stale_schema'
}

export async function getDraft(namespace: string): Promise<DraftSnapshot> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/draft`
  const body = objectIn(await request<unknown>(path), path)
  return {
    namespace: str(body.namespace, namespace),
    document: body.document ?? null,
    revision: num(body.revision),
    baseVersion: num(body.base_version),
    updatedBy: str(body.updated_by),
    updatedAt: str(body.updated_at),
  }
}

/**
 * Saves the draft against the revision it was read at.
 *
 * A `409 stale_revision` means someone else saved first. It arrives as a
 * thrown ApiError like every other refusal; `isStaleRevision` is how a caller
 * tells it apart, and nothing here retries or overwrites, because the HTTP
 * request itself carries no version but the one the caller read (CFG-B3).
 */
export async function saveDraft(
  namespace: string,
  document: unknown,
  revision: number,
): Promise<SavedDraft> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/draft`
  const body = objectIn(
    await request<unknown>(path, { method: 'PUT', body: { document, revision } }),
    path,
  )
  return { revision: num(body.revision), updatedAt: str(body.updated_at) }
}

export function isStaleRevision(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'stale_revision'
}

/**
 * A `409 no_changes` from version creation: the draft already IS the latest
 * version, so there is nothing to cut. It is a refusal rather than a failure, and
 * the create-version dialog shows the server's sentence inline instead of the
 * generic conflict path.
 */
export function isNoChanges(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'no_changes'
}

// ---------------------------------------------------------------------------
// Versions
//
// One immutable snapshot of a namespace's document per row. The list deliberately
// omits the body (up to 200 rows a page); the detail read carries it, and the
// diff endpoint returns both documents so a viewer can compare them without
// fetching each version.
// ---------------------------------------------------------------------------

export interface VersionSummary {
  version: number
  schemaVersion: number
  sha256: string
  message: string
  createdBy: string
  createdAt: string
}

export interface VersionDetail extends VersionSummary {
  size: number
  document: unknown
}

export interface VersionPage {
  versions: VersionSummary[]
  /** The cursor for the next, older page; null when this is the last page. */
  nextBefore: number | null
}

export interface VersionQuery {
  /** Only versions strictly below this number: the paging cursor. */
  before?: number
  limit?: number
}

function readVersion(row: Record<string, unknown>): VersionSummary {
  return {
    version: num(row.version),
    schemaVersion: num(row.schema_version),
    sha256: str(row.sha256),
    message: str(row.message),
    createdBy: str(row.created_by),
    createdAt: str(row.created_at),
  }
}

export async function listVersions(
  namespace: string,
  query: VersionQuery = {},
): Promise<VersionPage> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/versions`
  const body = objectIn(
    await request<unknown>(`${path}${queryString({ before: query.before, limit: query.limit })}`),
    path,
  )
  const next = body.next_before
  return {
    versions: recordsIn(body, 'versions', path).map(readVersion),
    nextBefore: typeof next === 'number' && Number.isFinite(next) ? next : null,
  }
}

export async function getVersion(namespace: string, version: number): Promise<VersionDetail> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/versions/${version}`
  const body = objectIn(await request<unknown>(path), path)
  return {
    ...readVersion(body),
    size: num(body.size),
    document: body.document ?? null,
  }
}

/**
 * Cuts a version from the server draft.
 *
 * The body carries the revision the editor reviewed the draft at, so a draft that
 * moved on is a `409 stale_revision` rather than a version of someone else's
 * document. A draft identical to the latest version is a `409 no_changes`; a
 * document the schema refuses is a `400 validation_failed`.
 */
export async function createVersion(
  namespace: string,
  input: { message: string; revision: number },
): Promise<VersionSummary> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/versions`
  const body = objectIn(await request<unknown>(path, { method: 'POST', body: input }), path)
  return {
    ...readVersion(body),
    // The create response has no document; the list shape is a summary, so the
    // reader only needs the fields it shares with a version row.
  }
}

/** One side of a diff, with the full document so a reader can render it. */
export interface DiffSide {
  ref: string
  document: unknown
}

/**
 * One structural change. Config's own op vocabulary (`add`/`remove`/`replace`)
 * is preserved rather than translated here, because the wire shape is the
 * service's and the display mapping belongs in the view.
 */
export interface DiffChange {
  op: 'add' | 'remove' | 'replace'
  path: string
  from?: unknown
  to?: unknown
}

export interface VersionDiff {
  from: DiffSide
  to: DiffSide
  changes: DiffChange[]
}

function readDiffSide(value: unknown, fallbackRef: string): DiffSide {
  if (isRecord(value)) return { ref: str(value.ref, fallbackRef), document: value.document ?? null }
  return { ref: str(value, fallbackRef), document: null }
}

function readChange(entry: unknown): DiffChange {
  const row = isRecord(entry) ? entry : {}
  const op = str(row.op) !== '' ? str(row.op) : kindToOp(str(row.kind))
  const change: DiffChange = {
    op: op === 'add' || op === 'remove' ? op : 'replace',
    path: str(row.path) !== '' ? str(row.path) : str(row.pointer),
    from: row.from !== undefined ? row.from : row.before,
    to: row.to !== undefined ? row.to : row.after,
  }
  return change
}

function kindToOp(kind: string): string {
  if (kind === 'added') return 'add'
  if (kind === 'removed') return 'remove'
  return 'replace'
}

/**
 * The server's diff between two refs, each a positive version number or `draft`.
 * Both full documents come back, which is what the side-by-side view needs.
 */
export async function getDiff(namespace: string, from: string, to: string): Promise<VersionDiff> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/diff`
  const url = `${path}${queryString({ from, to })}`
  const body = objectIn(await request<unknown>(url), path)
  return {
    from: readDiffSide(body.from, from),
    to: readDiffSide(body.to, to),
    changes: arrayIn(body, 'changes', path).map(readChange),
  }
}

/** Config's authoritative validation. The local run is a hint; this is not. */
export async function validateDocument(
  namespace: string,
  document: unknown,
): Promise<ValidationResult> {
  const path = `${BASE}/namespaces/${encodeURIComponent(namespace)}/draft/validate`
  const body = objectIn(await request<unknown>(path, { method: 'POST', body: { document } }), path)
  return {
    valid: bool(body.valid),
    errors: arrayIn(body, 'errors', path).map((entry) => {
      const row = isRecord(entry) ? entry : {}
      return { pointer: str(row.pointer), message: str(row.message) }
    }),
  }
}

// ---------------------------------------------------------------------------
// Releases
//
// The release history, the manifest snapshot a diff view reads, the publish the
// composer drives, and the two admin mutations that move a head: rollback to an
// earlier release of the same channel, and promote from the channel below.
// ---------------------------------------------------------------------------

/** One namespace entry of a manifest's `config` object. */
export interface ReleaseNamespace {
  version: number
  sha256: string
  size: number
}

/** One content pack of a manifest: the bytes a client downloads. */
export interface ReleasePack {
  name: string
  sha256: string
  size: number
}

/**
 * A released manifest, in the wire's snake_case read into camelCase. `config` is
 * keyed by namespace name, which is the whole point: a release names one version
 * per namespace and the diff is a comparison of those maps.
 */
export interface ReleaseManifest {
  format: number
  channel: string
  releaseId: number
  createdAt: string
  minClientVersion: string
  config: Record<string, ReleaseNamespace>
  packs: ReleasePack[]
}

/** One release row, with its manifest embedded. */
export interface Release {
  releaseId: number
  channel: string
  manifestSha256: string
  /**
   * The server manifest's hash, or null for a release made before CF-1. The
   * server document is only present from that feature on, so the two fields
   * travel together.
   */
  serverManifestSha256: string | null
  minClientVersion: string
  message: string
  createdBy: string
  createdAt: string
  manifest: ReleaseManifest
  /** The server-audience namespaces, or null for a pre-CF-1 release. */
  serverManifest: ReleaseManifest | null
}

export interface ReleaseQuery {
  /** Only releases strictly below this id: the paging cursor. */
  before?: number
  limit?: number
}

function queryString(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded === '' ? '' : `?${encoded}`
}

function readManifest(value: unknown): ReleaseManifest {
  const row = isRecord(value) ? value : {}
  const config: Record<string, ReleaseNamespace> = {}
  if (isRecord(row.config)) {
    for (const [namespace, entry] of Object.entries(row.config)) {
      const ns = isRecord(entry) ? entry : {}
      config[namespace] = {
        version: num(ns.version),
        sha256: str(ns.sha256),
        size: num(ns.size),
      }
    }
  }
  return {
    format: num(row.format),
    channel: str(row.channel),
    releaseId: num(row.release_id),
    createdAt: str(row.created_at),
    minClientVersion: str(row.min_client_version),
    config,
    packs: Array.isArray(row.packs)
      ? row.packs.filter(isRecord).map((pack) => ({
          name: str(pack.name),
          sha256: str(pack.sha256),
          size: num(pack.size),
        }))
      : [],
  }
}

function readRelease(row: Record<string, unknown>): Release {
  return {
    releaseId: num(row.release_id),
    channel: str(row.channel),
    manifestSha256: str(row.manifest_sha256),
    serverManifestSha256: nullableStr(row.server_manifest_sha256),
    minClientVersion: str(row.min_client_version),
    message: str(row.message),
    createdBy: str(row.created_by),
    createdAt: str(row.created_at),
    manifest: readManifest(row.manifest),
    serverManifest: isRecord(row.server_manifest) ? readManifest(row.server_manifest) : null,
  }
}

/** One history row: a release plus the head flag the endpoint states per row. */
export interface ReleaseHistoryRow extends Release {
  isHead: boolean
}

/**
 * A page of a channel's release history: the head once, the rows, and the
 * cursor for the older page (`GET /channels/{ch}/releases`).
 *
 * `head_release_id` is the authoritative head. The newest row is NOT the head
 * after a rollback, so a reader must mark the head by this id rather than by
 * position.
 */
export interface ReleaseHistoryPage {
  /** The channel's head release id, or 0 when the channel has no releases yet. */
  headReleaseId: number
  releases: ReleaseHistoryRow[]
  /** The cursor for the next, older page; null when this is the last page. */
  nextBefore: number | null
}

function readHistoryRow(row: Record<string, unknown>): ReleaseHistoryRow {
  return { ...readRelease(row), isHead: bool(row.is_head) }
}

/**
 * A channel's release history, newest first. The rows carry their manifests, so
 * this is also how one release is fetched: `getRelease` narrows the page to one.
 */
export async function listReleaseHistory(
  channel: string,
  query: ReleaseQuery = {},
): Promise<ReleaseHistoryPage> {
  const path = `${BASE}/channels/${encodeURIComponent(channel)}/releases`
  const url = `${path}${queryString({ before: query.before, limit: query.limit })}`
  const body = objectIn(await request<unknown>(url), path)
  const raw = body.head_release_id
  const next = body.next_before
  return {
    headReleaseId: typeof raw === 'number' && Number.isFinite(raw) ? raw : 0,
    releases: recordsIn(body, 'releases', path).map(readHistoryRow),
    nextBefore: typeof next === 'number' && Number.isFinite(next) ? next : null,
  }
}

/** The rows alone, for callers that only page a list. */
export async function listReleases(channel: string, query: ReleaseQuery = {}): Promise<Release[]> {
  return (await listReleaseHistory(channel, query)).releases
}

/**
 * One release, read by asking for the page that ends at it.
 *
 * `before=releaseId+1` is the trick: the endpoint returns releases strictly below
 * the cursor, so the page one above the wanted id starts with the wanted row. The
 * id is then checked rather than assumed, because a wrong id and a channel with
 * no such release both answer with a valid neighbouring page, and "not found"
 * has to be a refusal the caller can show, not the wrong release's diff.
 */
export async function getRelease(channel: string, releaseId: number): Promise<Release> {
  const releases = await listReleases(channel, { before: releaseId + 1, limit: 1 })
  const release = releases.find((candidate) => candidate.releaseId === releaseId)
  if (release === undefined) {
    throw new ApiError({
      status: 404,
      code: 'not_found',
      message: `release ${releaseId} is not a release of channel ${channel}`,
      requestId: null,
    })
  }
  return release
}

/** The release immediately below `releaseId` on the channel, or null when it is the first. */
export async function getPreviousRelease(
  channel: string,
  releaseId: number,
): Promise<Release | null> {
  const releases = await listReleases(channel, { before: releaseId, limit: 1 })
  return releases[0] ?? null
}

/** A channel's current head: the id the releases list states, and the row itself when it is in the page. */
export interface ChannelHead {
  /** The channel's head release id, or 0 when the channel has no releases yet. */
  headReleaseId: number
  release: Release | null
}

/**
 * The channel head, with its manifest.
 *
 * The history states `head_release_id` once, and after a rollback that head is
 * not the newest row, so the id is resolved against the page and the release is
 * fetched by id only when it fell outside the page. The id on its own is what a
 * publish sends as `base_release_id`.
 */
export async function getChannelHead(channel: string): Promise<ChannelHead> {
  const page = await listReleaseHistory(channel, { limit: 1 })
  if (page.headReleaseId === 0) return { headReleaseId: 0, release: null }
  const inPage = page.releases.find((candidate) => candidate.releaseId === page.headReleaseId)
  return {
    headReleaseId: page.headReleaseId,
    release: inPage ?? (await getRelease(channel, page.headReleaseId)),
  }
}

/**
 * The POST body a publish carries. `base_release_id` is the head the composer
 * previewed; packs are addressed by hash, never by name, because a pack's
 * identity is its bytes. The body deliberately repeats the snake_case wire
 * names: `versions` and `packs` are the API's vocabulary, and a composed
 * selection is exactly those two arrays.
 */
export interface PublishReleaseInput {
  base_release_id: number
  versions: { namespace: string; version: number }[]
  packs: { sha256: string }[]
  min_client_version: string
  message: string
}

/**
 * Publishes a release to a channel.
 *
 * A 201 carries the frozen release with its client and server manifests. The two
 * refusals a caller branches on are `409 stale_release` (the channel moved since
 * the composer read its head; `isStaleRelease`) and `400 validation_failed` (a
 * bad min client version or a server's own field rule), whose message is written
 * to be shown. Server-audience namespaces are accepted: the service sorts the
 * `versions` list by audience and builds both manifests.
 */
export async function publishRelease(
  channel: string,
  input: PublishReleaseInput,
): Promise<Release> {
  const path = `${BASE}/channels/${encodeURIComponent(channel)}/releases`
  return readRelease(objectIn(await request<unknown>(path, { method: 'POST', body: input }), path))
}

/** Someone published to the channel since the composer read its head. */
export function isStaleRelease(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'stale_release'
}

/** The rollback POST body: the earlier release to make head, and the head it was read against. */
export interface RollbackReleaseInput {
  release_id: number
  base_release_id: number
}

/**
 * Moves a channel's head pointer back to an earlier release of the same channel
 * (admin only). It writes no release row: the target snapshot is simply made
 * current again. A channel that moved past `base_release_id` is `409
 * stale_release`; the current head is `409 no_changes`; another channel's
 * release is `404 not_found`.
 */
export async function rollbackRelease(
  channel: string,
  input: RollbackReleaseInput,
): Promise<Release> {
  const path = `${BASE}/channels/${encodeURIComponent(channel)}/rollback`
  return readRelease(objectIn(await request<unknown>(path, { method: 'POST', body: input }), path))
}

/** The promote POST body: the target head the promotion was read against, plus its message. */
export interface PromoteReleaseInput {
  base_release_id: number
  message: string
}

/**
 * Copies the lower channel's head onto `channel` as a new release (admin only).
 * The `from` channel is a query parameter because the resource promoted INTO is
 * the path's channel. `409 stale_release` when the target moved, `409
 * no_changes` when it already carries the source content.
 */
export async function promoteRelease(
  channel: string,
  from: string,
  input: PromoteReleaseInput,
): Promise<Release> {
  const path = `${BASE}/channels/${encodeURIComponent(channel)}/promote`
  const url = `${path}${queryString({ from })}`
  return readRelease(objectIn(await request<unknown>(url, { method: 'POST', body: input }), path))
}

// ---------------------------------------------------------------------------
// Content packs
//
// A pack is uploaded as its raw .pck bytes rather than JSON, which is why it
// travels through `uploadWithProgress` and not `request`. The field names come
// from Config's `packResponse` (services/config/internal/api/packs.go): the
// wire says `uploaded_by`/`uploaded_at`, not the `created_*` of a version.
// ---------------------------------------------------------------------------

export interface PackSummary {
  packId: string
  name: string
  sha256: string
  size: number
  uploadedBy: string
  uploadedAt: string
}

function readPack(row: Record<string, unknown>): PackSummary {
  return {
    packId: str(row.pack_id),
    name: str(row.name),
    sha256: str(row.sha256),
    size: num(row.size),
    uploadedBy: str(row.uploaded_by),
    uploadedAt: str(row.uploaded_at),
  }
}

/** Every pack this deployment holds, newest first is the view's to sort. */
export async function listPacks(): Promise<PackSummary[]> {
  const path = `${BASE}/packs`
  return recordsIn(await request<unknown>(path), 'packs', path).map(readPack)
}

export interface UploadPackOptions {
  onProgress?: (progress: UploadProgress) => void
  signal?: AbortSignal
}

export interface PackUploadResult {
  pack: PackSummary
  /** 201: the bytes are new. 200: Config already held them (dedupe by sha256). */
  created: boolean
}

/**
 * Uploads one pack's bytes under `name`.
 *
 * The name goes in the query, not the body, because the body IS the file. The
 * endpoint answers 201 for a new pack and 200 when the same bytes already exist,
 * and the caller needs that distinction: the second case is a success that says
 * "already uploaded", not a duplicate error.
 */
export async function uploadPack(
  name: string,
  file: Blob,
  options: UploadPackOptions = {},
): Promise<PackUploadResult> {
  const resource = `${BASE}/packs`
  const path = `${resource}?name=${encodeURIComponent(name)}`
  const { status, data } = await uploadWithProgress<unknown>(path, file, {
    contentType: file.type === '' ? 'application/octet-stream' : file.type,
    onProgress: options.onProgress,
    signal: options.signal,
  })
  return { pack: readPack(objectIn(data, resource)), created: status === 201 }
}
