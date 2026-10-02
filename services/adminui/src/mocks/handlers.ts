import { http, HttpResponse } from 'msw'

// The same walker the SPA uses for the diff between an editor's draft and the
// one on the server. Shared rather than reimplemented, so the fixture cannot
// disagree with the app about the shape of a diff. `documentDiff` expresses the
// walker's result in Config's `{op, path, from, to}` wire vocabulary, which is
// what `GET /diff` documents.
import { documentDiff } from '@/config/diff'
import { isValidNamespaceName } from '@/config/namespaceName'
import { isValidPackName, PACK_MAGIC } from '@/config/packUpload'
import configAuditFixture from '@/mocks/fixtures/config-audit.json'
import configReleasesDevFixture from '@/mocks/fixtures/config-releases-dev.json'
import dashboardAuditFixture from '@/mocks/fixtures/dashboard-audit.json'
import dashboardLogsFixture from '@/mocks/fixtures/dashboard-logs.json'
import dashboardOverviewManyFixture from '@/mocks/fixtures/dashboard-overview-many.json'
import dashboardOverviewFixture from '@/mocks/fixtures/dashboard-overview.json'
import dashboardServicesFixture from '@/mocks/fixtures/dashboard-services.json'
import balanceWeaponsFixture from '@/mocks/fixtures/namespace-balance-weapons.json'
import serverTuningFixture from '@/mocks/fixtures/namespace-server-tuning.json'
import uiPresentationFixture from '@/mocks/fixtures/namespace-ui-presentation.json'

/**
 * The fixture API, in one place, because it is consumed three ways: the browser
 * under `npm run dev:mock`, Vitest through src/mocks/server.ts, and Playwright
 * against the dev server. There is deliberately no second implementation in a
 * Vite dev middleware.
 *
 * The paths are the ones the SPA really addresses, prefix and all. gateway_dev
 * forwards `/api/admin/...` unstripped, so nothing here is relative to a base
 * URL and nothing is rewritten on the way through.
 *
 * One caveat worth stating once: this mock does NOT enforce the role column in
 * design/02-config.md section 5. Roles are carried by the tokens so the SPA's role
 * gating (WEB-5, UX only) and the gateway's refusal can both be exercised, but a
 * viewer that PUTs a draft here gets a 200. Authorization is the gateway's job
 * and pretending otherwise in a fixture would hide a real 403 from the tests.
 *
 * Two families of design/02-config.md section 5 are deliberately absent rather
 * than forgotten: `POST /namespaces/{ns}/versions`, and the packs plus
 * channels/releases set. Both belong to the version and release composer, which
 * is CFG-D* and out of scope for the shell. A view that calls one of them gets
 * MSW's unhandled-request warning and then a connection error through the dev
 * proxy, which is the honest outcome: the backend does not exist yet either.
 * `POST /namespaces` and `PUT /schema` are here because the list's create dialog
 * and the schema page call them.
 */

const CONFIG = '/api/admin/config'
const DASHBOARD = '/api/admin/dashboard'
const AUTH = '/admin-auth'
const ACCOUNT = `${AUTH}/account`
const ADMIN_USERS = '/api/admin/users'

// ---------------------------------------------------------------------------
// COM-5 error envelope
// ---------------------------------------------------------------------------

type ErrorCode =
  | 'not_found'
  | 'precondition_required'
  | 'stale_schema'
  | 'invalid_credentials'
  | 'invalid_token'
  | 'invalid_link'
  | 'invalid_ticket'
  | 'invalid_code'
  | 'validation_failed'
  | 'stale_revision'
  | 'stale_release'
  | 'no_changes'
  | 'already_exists'
  | 'insufficient_role'
  | 'root_protected'
  | 'self_modification'
  | 'mfa_not_enrolled'
  | 'mfa_already_enabled'
  | 'mfa_not_allowed'
  | 'mfa_required'
  | 'invite_pending'
  | 'internal'

let requestSeq = 0

/**
 * A per-response id, not a trace id: nothing correlates it, but the SPA shows it
 * when something fails and staff paste it into a bug report, so it has to be
 * there and look like the real one.
 */
function requestId(): string {
  requestSeq += 1
  return `req_${requestSeq.toString(16).padStart(12, '0')}`
}

/** COM-5: exactly these three fields, from every endpoint, on every 4xx and 5xx. */
function fail(status: number, code: ErrorCode, message: string) {
  return HttpResponse.json({ error: { code, message, request_id: requestId() } }, { status })
}

async function readJson<T>(request: Request): Promise<T | null> {
  try {
    return (await request.json()) as T
  } catch {
    // A malformed body is the caller's problem, not a mock crash.
    return null
  }
}

/** MSW gives a path parameter as a string, unless a pattern repeats a name. */
function param(value: unknown): string {
  return Array.isArray(value) ? String(value[0] ?? '') : String(value ?? '')
}

// ---------------------------------------------------------------------------
// Staff auth
//
// These shapes are the proposals this branch codes the SPA against. PHP Admin
// Auth is not in this repo; when it lands, whoever writes it has to match this
// or the SPA changes. Both login and refresh return `user` WITH `roles` so the
// SPA never decodes the token to read a claim (WEB-2 forbids depending on a
// token's contents, and the token stays opaque).
// ---------------------------------------------------------------------------

interface SessionUser {
  id: string
  name: string
  roles: string[]
}

const ACCESS_TOKEN_TTL_SECONDS = 3600
const MFA_TICKET_PREFIX = 'mfa_ticket_'
/** An enrollment challenge, minted by login for an admin with no TOTP factor. */
const MFA_ENROLL_PREFIX = 'mfa_enroll_ticket_'

/**
 * Roles are derived from the email's local part so one fixture set can express
 * the role-gating test: `viewer@` sees the dashboard only, `liveops@` may save
 * drafts, `admin@` and anything else may do everything. `liveops` becomes
 * `live_ops` because the role string the gateway and Config use has the
 * underscore. Two local parts are reserved for flows rather than roles: `mfa`
 * asks for a second factor and `denied` refuses the sign-in.
 */
function rolesForEmail(email: string): string[] {
  const localPart = localPartOf(email)
  if (localPart.startsWith('viewer')) return ['viewer']
  if (localPart.startsWith('liveops')) return ['live_ops']
  return ['admin']
}

/** Split on the FIRST `@`: everything before it is the local part. */
function localPartOf(email: string): string {
  const at = email.indexOf('@')
  return (at === -1 ? email : email.slice(0, at)).toLowerCase()
}

function nameForEmail(email: string): string {
  const words = localPartOf(email)
    .split(/[._-]+/)
    .filter((word) => word.length > 0)
  if (words.length === 0) return email
  return words.map((word) => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function userForEmail(email: string): SessionUser {
  return { id: `usr_${localPartOf(email)}`, name: nameForEmail(email), roles: rolesForEmail(email) }
}

function encodeSegment(value: unknown): string {
  return btoa(JSON.stringify(value)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
}

/**
 * Shaped like a JWT, and useless as one: the third segment is a literal. It is
 * shaped that way so the SPA stores and forwards something that looks like what
 * production issues, and only that.
 */
function accessToken(user: SessionUser): string {
  const header = encodeSegment({ alg: 'HS256', typ: 'JWT' })
  const payload = encodeSegment({
    sub: user.id,
    name: user.name,
    roles: user.roles,
    exp: Math.floor(Date.now() / 1000) + ACCESS_TOKEN_TTL_SECONDS,
  })
  return `${header}.${payload}.not-a-real-signature`
}

/**
 * The mock has no session store, so it remembers the last login. That is enough
 * for the refresh and me flows, which are same-browser by definition.
 *
 * Mirrored into sessionStorage, which is not decoration. With the variable alone,
 * a page reload put the fixture back at "no session", so a signed-in visitor who
 * typed a URL was answered by the sign-in screen rather than by the guard: the
 * two are indistinguishable in the browser, and the reload path (the one
 * `restore()` exists for) could not be tested at all. The real server keeps the
 * refresh token in an httpOnly cookie the browser persists, and a service worker
 * cannot set that header, so the fixture persists the session itself instead.
 *
 * sessionStorage rather than localStorage: per tab, gone when the tab closes,
 * which is the right lifetime for a fixture and stops one test's sign-in from
 * deciding the next one's starting state.
 */
const SESSION_KEY = 'otomo.mock.session'

function loadSession(): SessionUser | null {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY)
    return raw === null ? null : (JSON.parse(raw) as SessionUser)
  } catch {
    // Unavailable (a blocked or partitioned storage) or unparseable. Either way
    // there is no remembered session, which is exactly what a fixture with no
    // storage should behave like.
    return null
  }
}

function remember(user: SessionUser | null): void {
  session = user
  try {
    if (user === null) sessionStorage.removeItem(SESSION_KEY)
    else sessionStorage.setItem(SESSION_KEY, JSON.stringify(user))
  } catch {
    // As above: the variable still holds it, so refresh works within this page.
  }
}

let session: SessionUser | null = loadSession()

function issuedSession(user: SessionUser) {
  remember(user)
  return { access_token: accessToken(user), expires_in: ACCESS_TOKEN_TTL_SECONDS, user }
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

interface DraftState {
  document: unknown
  revision: number
  base_version: number
  updated_by: string
  updated_at: string
}

interface VersionState {
  version: number
  schema_version: number
  message: string
  created_by: string
  created_at: string
  sha256: string
  document: unknown
}

interface NamespaceState {
  name: string
  audience: string
  description: string
  schema: { schema_version: number; body: unknown; created_by: string; created_at: string }
  draft: DraftState
  versions: VersionState[]
}

/**
 * The mock's whole world, and mutable: a draft save is visible to the next read,
 * which is what makes the optimistic-locking path testable (CFG-B3). The two
 * namespaces are the ones this branch needs: an ordinary one with a schema the
 * editor form is later built from, and the reserved `ui.presentation` one.
 */
const namespaces: NamespaceState[] = [
  balanceWeaponsFixture,
  serverTuningFixture,
  uiPresentationFixture,
]

/**
 * The content packs the fixture holds. Two are seeded so the list has rows for a
 * viewer without an upload first; a POST appends or, when the sha256 already
 * exists, answers with the existing row exactly as Config's dedupe does.
 */
interface PackState {
  pack_id: string
  name: string
  sha256: string
  size: number
  uploaded_by: string
  uploaded_at: string
}

const packs: PackState[] = [
  {
    pack_id: 'pack_seed_audio',
    name: 'audio_pack',
    sha256: '1a'.repeat(32),
    size: 1048576,
    uploaded_by: 'admin@example.com',
    uploaded_at: '2026-09-18T09:00:00Z',
  },
  {
    pack_id: 'pack_seed_weapons',
    name: 'weapons_pack',
    sha256: '2b'.repeat(32),
    size: 2621440,
    uploaded_by: 'liveops@example.com',
    uploaded_at: '2026-09-20T11:30:00Z',
  },
  // The two packs the seeded dev head names, so the composer can pre-check them.
  {
    pack_id: 'pack_seed_audio_pak',
    name: 'audio.pak',
    sha256: '22'.repeat(32),
    size: 4096,
    uploaded_by: 'admin@example.com',
    uploaded_at: '2026-09-18T09:00:00Z',
  },
  {
    pack_id: 'pack_seed_weapons_pak',
    name: 'weapons.pak',
    sha256: '11'.repeat(32),
    size: 2048,
    uploaded_by: 'liveops@example.com',
    uploaded_at: '2026-09-20T11:30:00Z',
  },
]

/** The real SHA-256 over the uploaded bytes, so dedupe is genuinely by content. */
async function sha256OfBytes(buffer: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', buffer)
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

function latestVersion(state: NamespaceState): VersionState | undefined {
  return state.versions[state.versions.length - 1]
}

/** The published-version fields a list or create response carries. */
function versionRow(version: VersionState) {
  return {
    version: version.version,
    schema_version: version.schema_version,
    message: version.message,
    created_by: version.created_by,
    created_at: version.created_at,
    sha256: version.sha256,
  }
}

/** Two-space JSON, the indentation Config canonicalises a document to. */
function serialiseDocument(value: unknown): string {
  return JSON.stringify(value ?? null, null, 2) ?? 'null'
}

/**
 * A version's SHA-256 stand-in: 64 hex characters, deterministic for the same
 * document. The fixture only has to be internally consistent — the short-sha
 * column reads the same value the create response returned — and it is
 * deliberately synchronous so a unit test does not race an async digest.
 */
function sha256Hex(text: string): string {
  const bytes = new TextEncoder().encode(text)
  let hash = 2166136261
  const out: string[] = []
  for (let index = 0; index < 32; index += 1) {
    hash ^= bytes[index % Math.max(bytes.length, 1)] ?? 0
    hash = Math.imul(hash, 16777619) >>> 0
    out.push((hash & 0xff).toString(16).padStart(2, '0'))
  }
  return out.join('')
}

function findNamespace(name: string): NamespaceState | undefined {
  return namespaces.find((candidate) => candidate.name === name)
}

/** The list-item shape both GET /namespaces and POST /namespaces answer with. */
function namespaceSummary(state: NamespaceState) {
  return {
    name: state.name,
    audience: state.audience,
    description: state.description,
    latest_version: latestVersion(state)?.version ?? 0,
    draft: {
      revision: state.draft.revision,
      updated_at: state.draft.updated_at,
      has_unpublished_changes: hasUnpublishedChanges(state),
    },
  }
}

const JSON_SCHEMA_TYPES = new Set([
  'null',
  'boolean',
  'object',
  'array',
  'number',
  'string',
  'integer',
])

/**
 * A stand-in for Config's `schema.Compile` (santhosh-tekuri/jsonschema).
 *
 * The real service rejects anything that is not a valid JSON Schema; the fixture
 * has to reject the case the schema page's tests exercise (`{"type": 12}`) and
 * otherwise stay out of the way, rather than growing into a second schema
 * implementation that keeps its own opinions about a project's document.
 */
function schemaCompileProblem(schema: unknown): string | null {
  if (typeof schema === 'boolean') return null
  if (typeof schema !== 'object' || schema === null || Array.isArray(schema)) {
    return 'at /: must be an object or boolean'
  }
  const record = schema as Record<string, unknown>
  if (!('type' in record)) return null

  const type = record.type
  if (typeof type === 'string') {
    if (!JSON_SCHEMA_TYPES.has(type)) return `at /type: unknown type ${JSON.stringify(type)}`
    return null
  }
  if (Array.isArray(type)) {
    if (type.length === 0) return 'at /type: must not be empty'
    for (const entry of type) {
      if (typeof entry !== 'string' || !JSON_SCHEMA_TYPES.has(entry)) {
        return `at /type: invalid type ${JSON.stringify(entry)}`
      }
    }
    return null
  }
  return 'at /type: must be a string or array of strings'
}

/** A draft that differs from the latest version is a draft with unpublished work. */
function hasUnpublishedChanges(state: NamespaceState): boolean {
  const latest = latestVersion(state)
  if (!latest) return true
  return JSON.stringify(state.draft.document) !== JSON.stringify(latest.document)
}

interface ValidationIssue {
  pointer: string
  message: string
}

interface PropertySchema {
  type?: string
  minimum?: number
  maximum?: number
  minLength?: number
}

/**
 * Deliberately shallow: required, type and numeric bounds for a flat object
 * schema, which is all the two fixtures use. The real Config validates with
 * santhosh-tekuri/jsonschema and the editor with the local validator (CFG-D3);
 * this exists so
 * the save path has something to disagree with before Config exists.
 */
function validateDocument(schema: unknown, document: unknown): ValidationIssue[] {
  if (typeof document !== 'object' || document === null || Array.isArray(document)) {
    return [{ pointer: '', message: 'must be an object' }]
  }

  const root = schema as { required?: string[]; properties?: Record<string, PropertySchema> }
  const record = document as Record<string, unknown>
  const issues: ValidationIssue[] = []

  for (const key of root.required ?? []) {
    if (!(key in record)) issues.push({ pointer: `/${key}`, message: 'is required' })
  }

  for (const [key, definition] of Object.entries(root.properties ?? {})) {
    if (!(key in record)) continue
    const value = record[key]
    if (!matchesType(definition.type, value)) {
      issues.push({
        pointer: `/${key}`,
        message: `must be of type ${definition.type ?? 'unknown'}`,
      })
      continue
    }
    if (typeof value === 'number') {
      if (definition.minimum !== undefined && value < definition.minimum) {
        issues.push({ pointer: `/${key}`, message: `must be >= ${definition.minimum}` })
      }
      if (definition.maximum !== undefined && value > definition.maximum) {
        issues.push({ pointer: `/${key}`, message: `must be <= ${definition.maximum}` })
      }
    }
    if (typeof value === 'string' && definition.minLength !== undefined) {
      if (value.length < definition.minLength) {
        issues.push({
          pointer: `/${key}`,
          message: `must be at least ${definition.minLength} characters`,
        })
      }
    }
  }

  return issues
}

function matchesType(type: string | undefined, value: unknown): boolean {
  switch (type) {
    case 'string':
      return typeof value === 'string'
    case 'integer':
      return typeof value === 'number' && Number.isInteger(value)
    case 'number':
      return typeof value === 'number'
    case 'boolean':
      return typeof value === 'boolean'
    case 'array':
      return Array.isArray(value)
    case 'object':
      return typeof value === 'object' && value !== null && !Array.isArray(value)
    default:
      return true
  }
}

/** `to=draft` is allowed (design/02-config.md section 5). */
function resolveDocument(state: NamespaceState, reference: string): unknown {
  if (reference === 'draft') return state.draft.document
  const version = Number(reference)
  return state.versions.find((candidate) => candidate.version === version)?.document
}

function pageOf<T>(entries: T[], pageSize: number, cursor: string | null) {
  const from = cursor === null || cursor === '' ? 0 : Number(cursor)
  if (!Number.isInteger(from) || from < 0) return null
  const page = entries.slice(from, from + pageSize)
  const next = from + page.length
  return { entries: page, next_cursor: next < entries.length ? String(next) : null }
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

/**
 * Named templates only: the browser never sends PromQL (design/01-dashboard.md
 * rule 2), so an unknown name is a 400 and not a passthrough. Each entry says
 * how many series the template yields and the unit those values carry.
 */
const SERIES_TEMPLATES: Record<
  string,
  {
    title: string
    unit: string
    series: string[]
    base: number
    amplitude: number
    decimals: number
  }
> = {
  rps_by_status: {
    title: 'Requests per second by status',
    unit: 'req/s',
    series: ['2xx', '3xx', '4xx', '5xx'],
    base: 8,
    amplitude: 6,
    decimals: 1,
  },
  error_ratio: {
    title: 'Error ratio',
    unit: '%',
    series: ['error_ratio'],
    base: 0.4,
    amplitude: 0.35,
    decimals: 3,
  },
  latency_p50: {
    title: 'Latency p50',
    unit: 'ms',
    series: ['p50'],
    base: 18,
    amplitude: 10,
    decimals: 0,
  },
  latency_p95: {
    title: 'Latency p95',
    unit: 'ms',
    series: ['p95'],
    base: 60,
    amplitude: 40,
    decimals: 0,
  },
  latency_p99: {
    title: 'Latency p99',
    unit: 'ms',
    series: ['p99'],
    base: 140,
    amplitude: 90,
    decimals: 0,
  },
  cpu: {
    title: 'CPU',
    unit: 'cores',
    series: ['cpu'],
    base: 0.3,
    amplitude: 0.25,
    decimals: 3,
  },
  memory: {
    title: 'Memory',
    unit: 'bytes',
    series: ['memory'],
    base: 268435456,
    amplitude: 134217728,
    decimals: 0,
  },
  goroutines: {
    title: 'Goroutines',
    unit: 'count',
    series: ['goroutines'],
    base: 220,
    amplitude: 120,
    decimals: 0,
  },
  heap_inuse: {
    title: 'Heap in use',
    unit: 'bytes',
    series: ['heap_inuse'],
    base: 67108864,
    amplitude: 33554432,
    decimals: 0,
  },
  gc_pause_max: {
    title: 'GC pause max',
    unit: 's',
    series: ['gc_pause_max'],
    base: 0.004,
    amplitude: 0.006,
    decimals: 4,
  },
}

const MAX_SERIES_POINTS = 1500
const MAX_SERIES_SECONDS = 7 * 24 * 60 * 60

/** Stable across requests in one session, so a chart does not jump on refresh. */
function seedOf(value: string): number {
  let seed = 0
  for (let index = 0; index < value.length; index += 1) {
    seed = (seed * 31 + value.charCodeAt(index)) % 997
  }
  return seed
}

function round(value: number, decimals: number): number {
  const factor = 10 ** decimals
  return Math.round(value * factor) / factor
}

/** Unix seconds, or a date: the columnar format in the doc uses epoch seconds. */
function parseTime(value: string | null, fallback: number): number {
  if (value === null || value === '') return fallback
  const numeric = Number(value)
  if (Number.isFinite(numeric)) return Math.floor(numeric)
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? fallback : Math.floor(parsed / 1000)
}

/**
 * The same, in milliseconds. The log endpoints carry instants, not the series
 * format's epoch seconds, and truncating them to a second would make a
 * sub-second boundary compare wrong: two lines can share a second.
 */
function parseTimeMs(value: string | null, fallback: number): number {
  if (value === null || value === '') return fallback
  const numeric = Number(value)
  if (Number.isFinite(numeric)) return Math.floor(numeric * 1000)
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? fallback : parsed
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, milliseconds)
  })
}

/**
 * Reads a browser storage flag without assuming storage exists.
 *
 * The same handler set runs under Node for the unit tests, where `localStorage`
 * may not be defined at all, and under a browser whose storage a privacy setting
 * can throw on. Either way the answer is "the flag is not set", which serves the
 * default fixture rather than crashing a mock handler.
 */
function safeStorageGet(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

/**
 * Ten thousand log lines for `/logs`, selected with the `otomo.mock.logs=many`
 * localStorage flag (set before boot by the e2e test). The default fixture is a
 * dozen lines, which cannot show whether the list virtualises; this can.
 *
 * Timestamps are spaced 300ms apart from a single anchor captured on first use,
 * so all ten thousand fall inside the default one-hour window and a page of
 * "load more" never overlaps the one before it because the clock moved. The
 * anchor is module state because the mock's world is per-page anyway.
 */
const MANY_LOG_COUNT = 10_000
const MANY_LOG_SPACING_MS = 300
/** End the fixture just before "now", so a range ending at now still includes it. */
const FIXTURE_LAG_MS = 30_000
let manyLogAnchor = 0
let stampedFixtureCache: Array<Record<string, unknown>> | null = null

function manyLogLines(): Array<Record<string, unknown>> {
  if (manyLogAnchor === 0) manyLogAnchor = Date.now() - FIXTURE_LAG_MS
  const services = ['config', 'gateway_dev', 'dashboard', 'admin-auth', 'patch']
  const levels = ['debug', 'info', 'info', 'warn', 'info', 'error']
  const lines: Array<Record<string, unknown>> = []
  for (let index = 0; index < MANY_LOG_COUNT; index += 1) {
    const line: Record<string, unknown> = {
      at: new Date(manyLogAnchor - index * MANY_LOG_SPACING_MS).toISOString(),
      service: services[index % services.length],
      level: levels[index % levels.length],
      message: `generated line ${index} service=${services[index % services.length]}`,
    }
    // One in seven carries a request id, so the follow-link path exists at scale.
    if (index % 7 === 0) {
      line.fields = {
        request_id: `req_many_${index.toString(16)}`,
        route: '/api/admin/dashboard/logs',
        duration_ms: index % 500,
      }
    }
    lines.push(line)
  }
  return lines
}

/**
 * The fixture is written with fixed timestamps, which read as dead the week
 * after it was authored. Slide the set so the newest line lands shortly before
 * now and the relative spacing is preserved; then a one-hour range actually
 * contains it, which is what makes the range filter testable at all.
 */
function stampedFixtureLines(): Array<Record<string, unknown>> {
  if (stampedFixtureCache !== null) return stampedFixtureCache
  const lines = dashboardLogsFixture.lines as Array<Record<string, unknown>>
  const newest = lines.length === 0 ? Date.now() : Date.parse(String(lines[0].at))
  if (Number.isNaN(newest)) {
    stampedFixtureCache = lines
    return stampedFixtureCache
  }
  const offset = Date.now() - FIXTURE_LAG_MS - newest
  stampedFixtureCache = lines.map((line) => ({
    ...line,
    at: new Date(Date.parse(String(line.at)) + offset).toISOString(),
  }))
  return stampedFixtureCache
}

/** The lines `/logs` reads: the fixture, or the ten-thousand-line set. */
function logLines(): Array<Record<string, unknown>> {
  return safeStorageGet('otomo.mock.logs') === 'many' ? manyLogLines() : stampedFixtureLines()
}

/** A tail rate in lines per second from localStorage, or 0 when unset/invalid. */
function tailRate(): number {
  const raw = safeStorageGet('otomo.mock.tailRate')
  if (raw === null || raw === '') return 0
  const rate = Number(raw)
  return Number.isFinite(rate) && rate > 0 ? rate : 0
}

/** A cap that is high enough to read as "open" for the length of any test. */
const GENERATED_TAIL_LIMIT = 1_000_000

/**
 * The live tail a rate flag asks for: real-time lines at `rate` per second,
 * generated rather than replayed, so a test can push the buffer hard. The loop
 * ends when the client disconnects (the enqueue throws) or, defensively, at the
 * cap.
 */
async function streamGenerated(
  controller: ReadableStreamDefaultController<Uint8Array>,
  encoder: TextEncoder,
  rate: number,
  matches: (line: { service: string; level: string; message: string }) => boolean,
): Promise<void> {
  const services = ['config', 'gateway_dev', 'dashboard', 'admin-auth', 'patch']
  const levels = ['info', 'debug', 'warn', 'error']
  const interval = Math.max(1000 / rate, 1)
  for (let seq = 0; seq < GENERATED_TAIL_LIMIT; seq += 1) {
    const line: {
      at: string
      service: string
      level: string
      message: string
      fields?: Record<string, unknown>
    } = {
      at: new Date().toISOString(),
      service: services[seq % services.length],
      level: levels[seq % levels.length],
      message: `tail line ${seq} service=${services[seq % services.length]}`,
    }
    if (seq % 5 === 0) line.fields = { request_id: `req_tail_${seq.toString(16)}` }
    if (matches(line)) controller.enqueue(encoder.encode(`data: ${JSON.stringify(line)}\n\n`))
    await delay(interval)
  }
}

// ---------------------------------------------------------------------------
// Channels and releases
//
// Mutable, so a publish is visible to the next read and the channel head can be
// bumped under a composer (the stale-release flow). `dev` is the fixture's
// history; `staging` and `live` start as copies of its head so all three
// channels have something to compose against.
// ---------------------------------------------------------------------------

interface ManifestConfigEntry {
  version: number
  sha256: string
  size: number
}

interface ManifestPackEntry {
  name: string
  sha256: string
  size: number
}

interface ManifestState {
  format: number
  channel: string
  release_id: number
  created_at: string
  min_client_version: string
  config: Record<string, ManifestConfigEntry>
  packs: ManifestPackEntry[]
}

interface ReleaseState {
  release_id: number
  channel: string
  manifest_sha256: string
  /** Null for the seeded pre-feature releases; set once one is published. */
  server_manifest_sha256: string | null
  min_client_version: string
  message: string
  created_by: string
  created_at: string
  manifest: ManifestState
  server_manifest: ManifestState | null
}

function cloneRelease(source: ReleaseState, channel: string, releaseId: number): ReleaseState {
  const copy = JSON.parse(JSON.stringify(source)) as ReleaseState
  copy.release_id = releaseId
  copy.channel = channel
  copy.manifest.channel = channel
  copy.manifest.release_id = releaseId
  if (copy.server_manifest !== null) {
    copy.server_manifest.channel = channel
    copy.server_manifest.release_id = releaseId
  }
  return copy
}

const devReleases = configReleasesDevFixture.releases as unknown as ReleaseState[]
// `staging` and `live` start behind dev rather than as copies of its head, so a
// promotion between channels has content to move and a live rollback has an
// earlier release to move back to. Both keep the fixture's release ids.
const stagingReleases = [cloneRelease(devReleases[2], 'staging', 1)]
const liveReleases = [
  cloneRelease(devReleases[1], 'live', 2),
  cloneRelease(devReleases[2], 'live', 1),
]

const channelReleases: Record<string, ReleaseState[]> = {
  dev: devReleases,
  staging: stagingReleases,
  live: liveReleases,
}

/**
 * The head pointer per channel. A rollback moves it to an earlier release of the
 * same channel without reordering the array or writing a row, exactly as the
 * store's `channel_head` does; the newest release id and the head are therefore
 * different things after one.
 */
const channelHead: Record<string, number> = {
  dev: devReleases[0].release_id,
  staging: stagingReleases[0].release_id,
  live: liveReleases[0].release_id,
}

function headOf(channel: string): number {
  return channelHead[channel] ?? 0
}

function findRelease(channel: string, releaseId: number): ReleaseState | undefined {
  return channelReleases[channel]?.find((release) => release.release_id === releaseId)
}

/** The head the store's manifest builder would produce, plus the release row metadata. */
function releaseResponse(release: ReleaseState) {
  return {
    release_id: release.release_id,
    channel: release.channel,
    manifest_sha256: release.manifest_sha256,
    server_manifest_sha256: release.server_manifest_sha256,
    min_client_version: release.min_client_version,
    message: release.message,
    created_by: release.created_by,
    created_at: release.created_at,
    manifest: release.manifest,
    server_manifest: release.server_manifest,
  }
}

// ---------------------------------------------------------------------------
// Staff user management (/api/admin/users)
//
// The real service enforces D3 in the handlers and again in the store; this
// fixture owes the SPA the same refusals so the page's error paths are exercised
// rather than hidden. The caller is the last login; the root flag comes from the
// matching row in `staffUsers`, exactly as `admin_auth` loads the caller from
// the database rather than trusting a token claim. A `root@` identity is seeded
// because there was none: without it the admin-role option and root's extra
// reach could not be tested end to end.
// ---------------------------------------------------------------------------

interface StaffState {
  id: string
  email: string
  name: string
  roles: string[]
  is_root: boolean
  status: 'active' | 'disabled'
  mfa_enrolled: boolean
  last_login_at: string | null
  created_at: string
}

interface InviteState {
  id: string
  purpose: 'invite' | 'reset'
  email: string
  name: string
  role: string | null
  user_id: string | null
  created_by: string
  created_at: string
  expires_at: string
}

const staffUsers: StaffState[] = [
  {
    id: 'usr_root',
    email: 'root@example.com',
    name: 'Root',
    roles: ['admin'],
    is_root: true,
    status: 'active',
    mfa_enrolled: true,
    last_login_at: '2026-09-24T08:15:00Z',
    created_at: '2026-01-04T09:00:00Z',
  },
  {
    id: 'usr_admin',
    email: 'admin@example.com',
    name: 'Admin',
    roles: ['admin'],
    is_root: false,
    status: 'active',
    mfa_enrolled: true,
    last_login_at: '2026-09-23T17:40:00Z',
    created_at: '2026-01-06T10:30:00Z',
  },
  {
    id: 'usr_liveops',
    email: 'liveops@example.com',
    name: 'Live Ops',
    roles: ['live_ops'],
    is_root: false,
    status: 'active',
    mfa_enrolled: false,
    last_login_at: '2026-09-22T12:05:00Z',
    created_at: '2026-02-11T09:45:00Z',
  },
  {
    id: 'usr_viewer',
    email: 'viewer@example.com',
    name: 'Viewer',
    roles: ['viewer'],
    is_root: false,
    status: 'active',
    mfa_enrolled: false,
    last_login_at: null,
    created_at: '2026-03-02T14:20:00Z',
  },
  {
    id: 'usr_disabled',
    email: 'disabled@example.com',
    name: 'Disabled Operator',
    roles: ['live_ops'],
    is_root: false,
    status: 'disabled',
    mfa_enrolled: true,
    last_login_at: '2026-08-30T09:00:00Z',
    created_at: '2026-04-18T08:00:00Z',
  },
]

let staffInvites: InviteState[] = []
let linkSeq = 0

/** Where `admin_auth`'s ADMIN_AUTH_PUBLIC_URL points in the fixture. */
const PUBLIC_URL = 'http://localhost:8090'

/** A stand-in for 32 random bytes; it only has to be unique within a test. */
function newLinkToken(): string {
  linkSeq += 1
  return `tok_${linkSeq}_${Math.random().toString(36).slice(2, 10)}`
}

function onboardURL(token: string): string {
  return `${PUBLIC_URL}/admin/onboard#token=${token}`
}

function staffByID(id: string): StaffState | undefined {
  return staffUsers.find((user) => user.id === id)
}

function callerStaff(): StaffState | undefined {
  return session === null ? undefined : staffByID(session.id)
}

function callerIsRoot(): boolean {
  return callerStaff()?.is_root === true
}

function hasAdminRole(roles: readonly string[]): boolean {
  return roles.includes('admin')
}

/** The admin gate `requireAdmin` puts in front of every route. */
function requireAdmin(): ReturnType<typeof fail> | null {
  if (session === null) return fail(401, 'invalid_token', 'not signed in')
  if (!session.roles.includes('admin')) {
    return fail(403, 'insufficient_role', 'the admin role is required')
  }
  return null
}

function staffResponse(user: StaffState) {
  return {
    id: user.id,
    email: user.email,
    name: user.name,
    roles: user.roles,
    is_root: user.is_root,
    status: user.status,
    mfa_enrolled: user.mfa_enrolled,
    last_login_at: user.last_login_at,
    created_at: user.created_at,
  }
}

function inviteResponse(invite: InviteState) {
  return {
    id: invite.id,
    purpose: invite.purpose,
    email: invite.email,
    name: invite.name,
    role: invite.role,
    created_by: invite.created_by,
    created_at: invite.created_at,
    expires_at: invite.expires_at,
  }
}

function inviteExpiry(): string {
  return new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString()
}

function createInvite(
  purpose: 'invite' | 'reset',
  email: string,
  name: string,
  role: string | null,
  userId: string | null,
  token: string,
): InviteState {
  linkSeq += 1
  const invite: InviteState = {
    id: `invite_${linkSeq}`,
    purpose,
    email,
    name,
    role,
    user_id: userId,
    created_by: session?.id ?? 'usr_admin',
    created_at: new Date().toISOString(),
    expires_at: inviteExpiry(),
  }
  staffInvites.push(invite)
  // The minted token is registered for onboarding. It goes through localStorage
  // rather than module state because the invite is redeemed in a *fresh tab*,
  // which has its own copy of this module (MSW runs the handlers in the page
  // that made the request). The real service is shared state; localStorage is
  // the fixture's closest stand-in, and the one thing that makes the
  // invite-from-the-UI -> redeem-in-a-new-page flow work end to end.
  registerMintedLink(token, {
    token,
    purpose,
    email,
    name,
    role,
    expired: false,
  })
  return invite
}

// ---------------------------------------------------------------------------
// Onboarding and TOTP enrollment
//
// The link tokens are fixed strings so the e2e suite can open a known URL:
//   mock-admin-invite    invite, role admin  -> enroll a first TOTP factor
//   mock-liveops-invite  invite, role live_ops -> signs straight in
//   mock-reset-mfa       reset for a TOTP user -> mfa_required
//   mock-expired         invite, already expired -> 404 invalid_link
//
// Enrollment accepts the fixed secret JBSWY3DPEHPK3PXP and the fixed code
// 123456, so an e2e run can complete the walk without a real authenticator.
// Every other code is a 401 invalid_code. This is a fixture: the real service
// verifies a real TOTP window against the secret it generated.
// ---------------------------------------------------------------------------

interface OnboardFixture {
  token: string
  purpose: 'invite' | 'reset'
  email: string
  name: string
  role: string | null
  expired: boolean
}

const ONBOARD_FIXTURES: OnboardFixture[] = [
  {
    token: 'mock-admin-invite',
    purpose: 'invite',
    email: 'new.admin@example.com',
    name: 'New Admin',
    role: 'admin',
    expired: false,
  },
  {
    token: 'mock-liveops-invite',
    purpose: 'invite',
    email: 'liveops.invite@example.com',
    name: 'New Live Ops',
    role: 'live_ops',
    expired: false,
  },
  {
    token: 'mock-reset-mfa',
    purpose: 'reset',
    email: 'mfa.ops@example.com',
    name: 'Mfa Ops',
    role: null,
    expired: false,
  },
  {
    token: 'mock-expired',
    purpose: 'invite',
    email: 'expired@example.com',
    name: 'Expired Person',
    role: 'admin',
    expired: true,
  },
]

/** The fixed secret and code the fixture's enrollment accepts. */
const MFA_FIXED_SECRET = 'JBSWY3DPEHPK3PXP'
const MFA_FIXED_CODE = '123456'
const MFA_RECOVERY_CODES = [
  'K7F2-9QDA',
  'M3P8-1LZX',
  'T6R4-0WVB',
  'H9N2-5ECY',
  'B1D7-8KJU',
  'Q4S6-3MPA',
  'X8C1-6RTN',
  'L2V9-7FHE',
  'G5Y3-0ZOW',
  'J0A4-2SDI',
]
const MOCK_COMMON_PASSWORDS = ['password', 'passw0rd', '123456', 'qwerty', 'letmein', 'welcome']

function findOnboardFixture(token: string): OnboardFixture | undefined {
  const fixed = ONBOARD_FIXTURES.find((fixture) => fixture.token === token)
  if (fixed !== undefined) return fixed
  return loadMintedLinks()[token]
}

/**
 * Tokens minted by an invite or a reset. Unlike the fixed `mock-*` fixtures,
 * these are created at runtime and have to survive into a second tab, which is
 * why they live in localStorage rather than module state. Storage can be absent
 * or throw (the Node server, a partitioned browser), in which case the link
 * still works within the tab that minted it.
 */
const MINTED_LINKS_KEY = 'otomo.mock.minted_links'

function loadMintedLinks(): Record<string, OnboardFixture> {
  try {
    const raw = localStorage.getItem(MINTED_LINKS_KEY)
    return raw === null ? {} : (JSON.parse(raw) as Record<string, OnboardFixture>)
  } catch {
    return {}
  }
}

function registerMintedLink(token: string, fixture: OnboardFixture): void {
  try {
    const all = loadMintedLinks()
    all[token] = fixture
    localStorage.setItem(MINTED_LINKS_KEY, JSON.stringify(all))
  } catch {
    // No shared storage: the token is still resolvable if the link is opened in
    // the same tab, which is what the unit tests exercise.
  }
}

/** The same three rules the server's password policy names, so a 400 reads true. */
function onboardPasswordProblem(password: string, email: string): string | null {
  if (password.length < 12) return 'password must be at least 12 characters'
  const lower = password.toLowerCase()
  if (MOCK_COMMON_PASSWORDS.some((word) => lower.includes(word))) {
    return 'password is one of the most common passwords'
  }
  if (email !== '' && lower.includes(localPartOf(email))) {
    return 'password must not contain the email address'
  }
  return null
}

function enrollTicketFor(email: string): string {
  return `${MFA_ENROLL_PREFIX}${email}`
}

// ---------------------------------------------------------------------------
// Account
//
// Stateful on purpose: the page changes a password, turns a factor on and off,
// and revokes sessions, and the assertions after each are about the new state
// rather than the response alone. The session endpoints are keyed by user id and
// seeded with two "other" sessions, so "sign out other sessions" has something
// to revoke without a second browser.
// ---------------------------------------------------------------------------

interface AccountSessionState {
  id: string
  created_at: string
  last_used_at: string
  ip: string
  user_agent: string
  current: boolean
}

const CHROME_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36'
const FIREFOX_LINUX = 'Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0'
const SAFARI_IPHONE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1'

const accountSessions = new Map<string, AccountSessionState[]>()
const accountPasswords = new Map<string, string>()
/** MFA state for accounts that have no `staff_user` fixture row. */
const extraMfa = new Map<string, boolean>()
let accountSessionSeq = 0

function localFromUser(user: SessionUser): string {
  return user.id.startsWith('usr_') ? user.id.slice(4) : ''
}

function emailForUser(user: SessionUser): string {
  return staffByID(user.id)?.email ?? `${localFromUser(user)}@example.com`
}

function isRootUser(user: SessionUser): boolean {
  return staffByID(user.id)?.is_root === true
}

/**
 * The factor bit for the fixture. The seeded staff rows own it where they exist;
 * otherwise the enrolment-flow local parts decide, matching the login branches
 * that already reserve `mfa` and `enroll`.
 */
function mfaEnrolledFor(user: SessionUser): boolean {
  const staff = staffByID(user.id)
  if (staff) return staff.mfa_enrolled
  const known = extraMfa.get(user.id)
  if (known !== undefined) return known
  const local = localFromUser(user)
  return local.startsWith('mfa') || local.startsWith('enroll')
}

function setMfaEnrolledFor(user: SessionUser, value: boolean): void {
  const staff = staffByID(user.id)
  if (staff) staff.mfa_enrolled = value
  else extraMfa.set(user.id, value)
}

function sessionsForUser(user: SessionUser): AccountSessionState[] {
  let list = accountSessions.get(user.id)
  if (list === undefined) {
    const now = Date.now()
    accountSessionSeq += 1
    list = [
      {
        id: `sess_${accountSessionSeq}_current`,
        created_at: new Date(now - 40 * 60 * 1000).toISOString(),
        last_used_at: new Date(now).toISOString(),
        ip: '203.0.113.7',
        user_agent: CHROME_MAC,
        current: true,
      },
      {
        id: `sess_${accountSessionSeq}_other`,
        created_at: new Date(now - 26 * 60 * 60 * 1000).toISOString(),
        last_used_at: new Date(now - 3 * 60 * 60 * 1000).toISOString(),
        ip: '198.51.100.24',
        user_agent: FIREFOX_LINUX,
        current: false,
      },
      {
        id: `sess_${accountSessionSeq}_phone`,
        created_at: new Date(now - 5 * 24 * 60 * 60 * 1000).toISOString(),
        last_used_at: new Date(now - 2 * 24 * 60 * 60 * 1000).toISOString(),
        ip: '203.0.113.99',
        user_agent: SAFARI_IPHONE,
        current: false,
      },
    ]
    accountSessions.set(user.id, list)
  }
  return list
}

function enrollPayload(label: string) {
  const issuer = encodeURIComponent('Otomo Admin')
  return {
    secret: MFA_FIXED_SECRET,
    otpauth_url: `otpauth://totp/${issuer}:${encodeURIComponent(label)}?secret=${MFA_FIXED_SECRET}&issuer=${issuer}`,
  }
}

export const handlers = [
  // -------------------------------------------------------------------------
  // Staff auth
  // -------------------------------------------------------------------------

  http.post(`${AUTH}/login`, async ({ request }) => {
    const body = await readJson<{ email?: string; password?: string }>(request)
    if (typeof body?.email !== 'string' || body.email.length === 0) {
      return fail(400, 'validation_failed', 'email is required')
    }
    if (typeof body.password !== 'string' || body.password.length === 0) {
      return fail(400, 'validation_failed', 'password is required')
    }

    // A local part starting with `denied` is refused, so the failure path has a
    // way through the fixtures. Everything else that is not `mfa` succeeds, and
    // deliberately without a magic password: a fixture set where the ordinary
    // case needs one puts that password in every test that signs in.
    if (localPartOf(body.email).startsWith('denied')) {
      return fail(401, 'invalid_credentials', 'no account matches those credentials')
    }

    // A password changed on the account page replaces the fixture's default
    // "anything goes"; before that, any password is accepted so tests that only
    // need a session do not carry a magic string.
    const loginUser = userForEmail(body.email)
    const storedPassword = accountPasswords.get(loginUser.id)
    if (storedPassword !== undefined && body.password !== storedPassword) {
      return fail(401, 'invalid_credentials', 'no account matches those credentials')
    }

    // A local part starting with `mfa` is the account with a second factor, so
    // the MFA screen and the login screen both have a path through the fixtures.
    if (localPartOf(body.email).startsWith('mfa')) {
      return HttpResponse.json({
        mfa_required: true,
        mfa_ticket: `${MFA_TICKET_PREFIX}${body.email}`,
      })
    }

    // An admin with no confirmed TOTP factor has to enroll before a session is
    // issued. The local part `enroll` is the fixture's way to reach that branch.
    if (localPartOf(body.email).startsWith('enroll')) {
      return HttpResponse.json({
        mfa_enrollment_required: true,
        mfa_ticket: enrollTicketFor(body.email),
      })
    }

    return HttpResponse.json(issuedSession(userForEmail(body.email)))
  }),

  // The onboarding pair. The token rides in the body, matching the contract and
  // the SPA's fragment-only handling.
  http.post(`${AUTH}/onboard/lookup`, async ({ request }) => {
    const body = await readJson<{ token?: string }>(request)
    if (typeof body?.token !== 'string' || body.token.length === 0) {
      return fail(400, 'validation_failed', 'token must not be empty')
    }
    const fixture = findOnboardFixture(body.token)
    if (fixture === undefined || fixture.expired) {
      return fail(404, 'invalid_link', 'this link is invalid or has expired')
    }
    return HttpResponse.json({
      purpose: fixture.purpose,
      email: fixture.email,
      name: fixture.name,
      role: fixture.role,
      expires_at: new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString(),
    })
  }),

  http.post(`${AUTH}/onboard`, async ({ request }) => {
    const body = await readJson<{ token?: string; password?: string }>(request)
    if (typeof body?.token !== 'string' || typeof body.password !== 'string') {
      return fail(400, 'validation_failed', 'token and password are required')
    }
    const fixture = findOnboardFixture(body.token)
    if (fixture === undefined || fixture.expired) {
      return fail(404, 'invalid_link', 'this link is invalid or has expired')
    }

    const problem = onboardPasswordProblem(body.password, fixture.email)
    if (problem !== null) return fail(400, 'validation_failed', problem)

    // A reset for a TOTP user stops at the factor prompt; an admin invite has no
    // factor yet, so it goes to enrollment; everyone else signs straight in.
    if (fixture.purpose === 'reset') {
      if (localPartOf(fixture.email).startsWith('mfa')) {
        return HttpResponse.json({
          mfa_required: true,
          mfa_ticket: `${MFA_TICKET_PREFIX}${fixture.email}`,
        })
      }
      return HttpResponse.json(issuedSession(userForEmail(fixture.email)))
    }

    if (fixture.role === 'admin') {
      return HttpResponse.json({
        mfa_enrollment_required: true,
        mfa_ticket: enrollTicketFor(fixture.email),
      })
    }

    return HttpResponse.json(issuedSession(userForEmail(fixture.email)))
  }),

  http.post(`${AUTH}/mfa/verify`, async ({ request }) => {
    const body = await readJson<{ mfa_ticket?: string; code?: string }>(request)
    if (typeof body?.mfa_ticket !== 'string' || !body.mfa_ticket.startsWith(MFA_TICKET_PREFIX)) {
      return fail(400, 'validation_failed', 'mfa_ticket is not a ticket this server issued')
    }
    if (typeof body.code !== 'string' || body.code.length === 0) {
      return fail(400, 'validation_failed', 'code is required')
    }

    // Any non-empty code is accepted. A fixed one would only make the E2E test
    // look like it proves something about the second factor.
    const email = body.mfa_ticket.slice(MFA_TICKET_PREFIX.length)
    return HttpResponse.json(issuedSession(userForEmail(email)))
  }),

  // TOTP enrollment. The fixture's secret and accepted code are fixed and
  // documented above; the real service generates a secret and checks the window.
  // A ticket is the sign-in challenge; an empty body is the account page's
  // bearer mode, where the session answers "not enrolled" with a secret and
  // "already enrolled" with 409.
  http.post(`${AUTH}/mfa/enroll`, async ({ request }) => {
    const body = await readJson<{ mfa_ticket?: string }>(request)
    if (typeof body?.mfa_ticket === 'string' && body.mfa_ticket.length > 0) {
      if (!body.mfa_ticket.startsWith(MFA_ENROLL_PREFIX)) {
        return fail(401, 'invalid_ticket', 'this sign-in attempt has expired; sign in again')
      }
      const email = body.mfa_ticket.slice(MFA_ENROLL_PREFIX.length)
      return HttpResponse.json(enrollPayload(email))
    }

    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    if (isRootUser(session)) {
      return fail(403, 'mfa_not_allowed', 'the break-glass root account cannot use MFA')
    }
    if (mfaEnrolledFor(session)) {
      return fail(409, 'mfa_already_enabled', 'this account already has a confirmed second factor')
    }
    return HttpResponse.json(enrollPayload(session.name))
  }),

  http.post(`${AUTH}/mfa/confirm`, async ({ request }) => {
    const body = await readJson<{ mfa_ticket?: string; code?: string }>(request)
    if (typeof body?.mfa_ticket === 'string' && body.mfa_ticket.length > 0) {
      if (!body.mfa_ticket.startsWith(MFA_ENROLL_PREFIX)) {
        return fail(401, 'invalid_ticket', 'this sign-in attempt has expired; sign in again')
      }
      if (body.code !== MFA_FIXED_CODE) {
        return fail(401, 'invalid_code', 'the code is not valid')
      }
      const email = body.mfa_ticket.slice(MFA_ENROLL_PREFIX.length)
      const user = userForEmail(email)
      setMfaEnrolledFor(user, true)
      // Confirming with a ticket also issues the session, as the contract says:
      // enrollment ends signed in.
      return HttpResponse.json({
        recovery_codes: MFA_RECOVERY_CODES,
        ...issuedSession(user),
      })
    }

    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    if (isRootUser(session)) {
      return fail(403, 'mfa_not_allowed', 'the break-glass root account cannot use MFA')
    }
    if (mfaEnrolledFor(session)) {
      return fail(409, 'mfa_already_enabled', 'this account already has a confirmed second factor')
    }
    if (body?.code !== MFA_FIXED_CODE) {
      return fail(401, 'invalid_code', 'the code is not valid')
    }
    setMfaEnrolledFor(session, true)
    return HttpResponse.json({ recovery_codes: MFA_RECOVERY_CODES })
  }),

  // No cookie in a fixture: the session is whatever the last login established,
  // so refresh works within one browser session and 401s before the first login.
  http.post(`${AUTH}/refresh`, () => {
    if (!session) return fail(401, 'invalid_credentials', 'no session to refresh')
    return HttpResponse.json(issuedSession(session))
  }),

  http.post(`${AUTH}/logout`, () => {
    remember(null)
    return new HttpResponse(null, { status: 204 })
  }),

  http.get(`${AUTH}/me`, () => {
    if (!session) return fail(401, 'invalid_credentials', 'not signed in')
    // Read from the staff state, not the token, so the account page's view of
    // `is_root` and `mfa_enabled` changes when enrollment is confirmed or turned
    // off, exactly as the DB-backed service does.
    return HttpResponse.json({
      id: session.id,
      name: session.name,
      roles: session.roles,
      is_root: isRootUser(session),
      mfa_enabled: mfaEnrolledFor(session),
    })
  }),

  // PHP Admin Auth's own audit, the third source Dashboard's merged page fans
  // out to. Same rows as the merged fixture's `admin-auth` entries, without the
  // `source` field Dashboard adds: `admin_auth`'s table is not shared and does
  // not carry it. A future Admin Auth page reads this directly.
  http.get(`${AUTH}/audit`, ({ request }) => {
    if (!session) return fail(401, 'invalid_credentials', 'not signed in')
    const url = new URL(request.url)
    const rows = dashboardAuditFixture.entries
      .filter((entry) => entry.source === 'admin-auth')
      .map((entry) => ({
        id: entry.id,
        at: entry.at,
        actor_id: entry.actor_id,
        actor_name: entry.actor_name,
        action: entry.action,
        target: entry.target,
        details: entry.details,
      }))
    const page = pageOf(rows, 5, url.searchParams.get('cursor'))
    if (page === null) return fail(400, 'validation_failed', 'cursor is not a page cursor')
    return HttpResponse.json(page)
  }),

  // -------------------------------------------------------------------------
  // Account
  // -------------------------------------------------------------------------

  http.post(`${ACCOUNT}/password`, async ({ request }) => {
    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    const body = await readJson<{ current_password?: string; new_password?: string }>(request)
    if (typeof body?.current_password !== 'string' || body.current_password.length === 0) {
      return fail(400, 'validation_failed', 'current_password must not be empty')
    }
    if (typeof body.new_password !== 'string' || body.new_password.length === 0) {
      return fail(400, 'validation_failed', 'new_password must not be empty')
    }
    const known = accountPasswords.get(session.id)
    if (known !== undefined && body.current_password !== known) {
      return fail(401, 'invalid_credentials', 'the current password is not correct')
    }
    const problem = onboardPasswordProblem(body.new_password, emailForUser(session))
    if (problem !== null) return fail(400, 'validation_failed', problem)

    accountPasswords.set(session.id, body.new_password)
    // The server revokes every other refresh family for a password change.
    accountSessions.set(
      session.id,
      sessionsForUser(session).filter((entry) => entry.current),
    )
    return new HttpResponse(null, { status: 204 })
  }),

  http.get(`${ACCOUNT}/sessions`, () => {
    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    return HttpResponse.json({ sessions: sessionsForUser(session) })
  }),

  http.post(`${ACCOUNT}/sessions/revoke-others`, () => {
    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    const list = sessionsForUser(session)
    const revoked = list.filter((entry) => !entry.current).length
    accountSessions.set(
      session.id,
      list.filter((entry) => entry.current),
    )
    return HttpResponse.json({ revoked, current_kept: true })
  }),

  http.post(`${ACCOUNT}/mfa/recovery-codes`, async ({ request }) => {
    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    if (!mfaEnrolledFor(session)) {
      return fail(409, 'mfa_not_enrolled', 'the account has no second factor to reset')
    }
    const body = await readJson<{ code?: string }>(request)
    if (body?.code !== MFA_FIXED_CODE) {
      return fail(401, 'invalid_code', 'the code is not valid')
    }
    return HttpResponse.json({ recovery_codes: MFA_RECOVERY_CODES })
  }),

  http.post(`${ACCOUNT}/mfa/disable`, async ({ request }) => {
    if (session === null) return fail(401, 'invalid_token', 'not signed in')
    if (isRootUser(session)) {
      return fail(403, 'root_protected', 'the root account can only be changed through the CLI')
    }
    if (session.roles.includes('admin')) {
      return fail(403, 'mfa_required', 'two-factor authentication is required for admin accounts')
    }
    if (!mfaEnrolledFor(session)) {
      return fail(409, 'mfa_not_enrolled', 'the account has no second factor to disable')
    }
    const body = await readJson<{ code?: string }>(request)
    if (body?.code !== MFA_FIXED_CODE) {
      return fail(401, 'invalid_code', 'the code is not valid')
    }
    setMfaEnrolledFor(session, false)
    return new HttpResponse(null, { status: 204 })
  }),

  // -------------------------------------------------------------------------
  // Staff user management (/api/admin/users)
  // -------------------------------------------------------------------------

  http.get(`${ADMIN_USERS}`, () => {
    const denied = requireAdmin()
    if (denied) return denied
    return HttpResponse.json({ users: staffUsers.map(staffResponse) })
  }),

  http.post(`${ADMIN_USERS}`, async ({ request }) => {
    const denied = requireAdmin()
    if (denied) return denied

    const body = await readJson<{ email?: unknown; name?: unknown; role?: unknown }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')
    const email = typeof body.email === 'string' ? body.email.trim() : ''
    const name = typeof body.name === 'string' ? body.name.trim() : ''
    const role = typeof body.role === 'string' ? body.role : ''
    if (email.length < 3 || email.length > 254 || email.split('@').length !== 2) {
      return fail(400, 'validation_failed', 'email must contain exactly one @')
    }
    if (name === '') return fail(400, 'validation_failed', 'name is required')
    if (role !== 'viewer' && role !== 'live_ops' && role !== 'admin') {
      return fail(400, 'validation_failed', 'role must be one of viewer, live_ops, admin')
    }
    // D3: granting admin requires root.
    if (role === 'admin' && !callerIsRoot()) {
      return fail(403, 'insufficient_role', 'only root can grant admin')
    }
    if (staffUsers.some((user) => user.email.toLowerCase() === email.toLowerCase())) {
      return fail(409, 'already_exists', 'a staff user already has that email')
    }
    if (staffInvites.some((invite) => invite.email.toLowerCase() === email.toLowerCase())) {
      return fail(409, 'invite_pending', 'a pending invite already exists for that email')
    }

    const token = newLinkToken()
    const invite = createInvite('invite', email, name, role, null, token)
    return HttpResponse.json(
      {
        invite_id: invite.id,
        email,
        role,
        invite_url: onboardURL(token),
        expires_at: invite.expires_at,
      },
      { status: 201 },
    )
  }),

  http.get(`${ADMIN_USERS}/invites`, () => {
    const denied = requireAdmin()
    if (denied) return denied
    return HttpResponse.json({ invites: staffInvites.map(inviteResponse) })
  }),

  http.delete(`${ADMIN_USERS}/invites/:id`, ({ params }) => {
    const denied = requireAdmin()
    if (denied) return denied
    const id = param(params.id)
    const index = staffInvites.findIndex((invite) => invite.id === id)
    if (index === -1) return fail(404, 'not_found', 'no such pending invite')
    staffInvites.splice(index, 1)
    return new HttpResponse(null, { status: 204 })
  }),

  http.patch(`${ADMIN_USERS}/:id`, async ({ params, request }) => {
    const denied = requireAdmin()
    if (denied) return denied

    const body = await readJson<{ roles?: unknown; status?: unknown }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')

    const hasRoles = body.roles !== undefined
    const hasStatus = body.status !== undefined
    if (!hasRoles && !hasStatus) {
      return fail(400, 'validation_failed', 'roles or status is required')
    }

    let roles: string[] | null = null
    if (hasRoles) {
      if (!Array.isArray(body.roles) || body.roles.length === 0) {
        return fail(400, 'validation_failed', 'roles must not be empty')
      }
      roles = body.roles.filter((role): role is string => typeof role === 'string')
      if (roles.some((role) => role !== 'viewer' && role !== 'live_ops' && role !== 'admin')) {
        return fail(400, 'validation_failed', 'roles must be a subset of viewer, live_ops, admin')
      }
    }
    if (hasStatus && body.status !== 'active' && body.status !== 'disabled') {
      return fail(400, 'validation_failed', 'status must be active or disabled')
    }

    const target = staffByID(param(params.id))
    if (target === undefined) return fail(404, 'not_found', 'no such user')
    if (target.is_root) {
      return fail(403, 'root_protected', 'the root account can only be changed through the CLI')
    }
    if (target.id === session?.id) {
      return fail(403, 'self_modification', 'you cannot modify your own account')
    }

    if (roles !== null) {
      // Granting or removing admin is root-only, whether or not the request
      // touches any other role.
      if (hasAdminRole(target.roles) !== hasAdminRole(roles) && !callerIsRoot()) {
        return fail(403, 'insufficient_role', 'only root can change admin access')
      }
      target.roles = roles
    }
    if (hasStatus) {
      const status = body.status as 'active' | 'disabled'
      if (status !== target.status && hasAdminRole(target.roles) && !callerIsRoot()) {
        return fail(403, 'insufficient_role', 'only root can change an admin status')
      }
      target.status = status
    }
    return HttpResponse.json(staffResponse(target))
  }),

  http.post(`${ADMIN_USERS}/:id/reset`, ({ params }) => {
    const denied = requireAdmin()
    if (denied) return denied
    const target = staffByID(param(params.id))
    if (target === undefined) return fail(404, 'not_found', 'no such user')
    if (target.is_root) {
      return fail(403, 'root_protected', 'the root account can only be changed through the CLI')
    }
    if (hasAdminRole(target.roles) && !callerIsRoot()) {
      return fail(403, 'insufficient_role', 'only root can reset an admin password')
    }

    // Only the newest reset link for an account works, as the store does.
    staffInvites = staffInvites.filter(
      (invite) => !(invite.purpose === 'reset' && invite.user_id === target.id),
    )
    const token = newLinkToken()
    const invite = createInvite('reset', target.email, target.name, null, target.id, token)
    return HttpResponse.json(
      { invite_id: invite.id, reset_url: onboardURL(token), expires_at: invite.expires_at },
      { status: 201 },
    )
  }),

  http.post(`${ADMIN_USERS}/:id/mfa/reset`, ({ params }) => {
    const denied = requireAdmin()
    if (denied) return denied
    const target = staffByID(param(params.id))
    if (target === undefined) return fail(404, 'not_found', 'no such user')
    // The store's check order: root, then self, then the root-only admin rule.
    if (target.is_root) return fail(403, 'root_protected', 'root has no second factor')
    if (target.id === session?.id) {
      return fail(403, 'self_modification', 'you cannot reset your own second factor')
    }
    if (hasAdminRole(target.roles) && !callerIsRoot()) {
      return fail(403, 'insufficient_role', "only root can reset an admin's second factor")
    }
    if (!target.mfa_enrolled) {
      return fail(409, 'mfa_not_enrolled', 'the account has no second factor to reset')
    }
    target.mfa_enrolled = false
    return HttpResponse.json(staffResponse(target))
  }),

  // -------------------------------------------------------------------------
  // Config
  // -------------------------------------------------------------------------

  http.get(`${CONFIG}/namespaces`, () => {
    return HttpResponse.json({ namespaces: namespaces.map(namespaceSummary) })
  }),

  http.post(`${CONFIG}/namespaces`, async ({ request }) => {
    const body = await readJson<{ name?: unknown; audience?: unknown; description?: unknown }>(
      request,
    )
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')

    const name = typeof body.name === 'string' ? body.name : ''
    const audience = typeof body.audience === 'string' ? body.audience : ''
    const description = typeof body.description === 'string' ? body.description : ''

    // Field order mirrors Config's validateCreate, so the first problem named is
    // the first field a form presents.
    if (name === '') return fail(400, 'validation_failed', 'name is required')
    if (name.length > 64) {
      return fail(400, 'validation_failed', 'name must be at most 64 characters')
    }
    if (!isValidNamespaceName(name)) {
      return fail(
        400,
        'validation_failed',
        'name must match ^[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]*)*$',
      )
    }
    if (audience === '') return fail(400, 'validation_failed', 'audience is required')
    if (audience !== 'client' && audience !== 'server') {
      return fail(400, 'validation_failed', 'audience must be "client" or "server"')
    }
    if (description.length > 500) {
      return fail(400, 'validation_failed', 'description must be at most 500 characters')
    }
    if (findNamespace(name)) {
      return fail(409, 'already_exists', 'a namespace with that name already exists')
    }

    const now = new Date().toISOString()
    const actor = session?.id ?? 'usr_admin'
    const state: NamespaceState = {
      name,
      audience,
      description,
      // CreateNamespace seeds an empty-object schema at version 1 (see
      // services/config/internal/store/namespaces.go).
      schema: { schema_version: 1, body: {}, created_by: actor, created_at: now },
      draft: {
        document: {},
        revision: 1,
        base_version: 0,
        updated_by: session?.name ?? 'Admin',
        updated_at: now,
      },
      versions: [],
    }
    namespaces.push(state)
    return HttpResponse.json(namespaceSummary(state), { status: 201 })
  }),

  // Content packs. The body is the raw .pck bytes, so these are the only
  // Config handlers here that do not read JSON: the name travels in the query
  // and the sha256 is computed over the bytes, which is what makes the second
  // upload of the same file a 200 with the first one's row rather than a 409.
  http.get(`${CONFIG}/packs`, () => {
    return HttpResponse.json({
      packs: [...packs].sort((left, right) => right.uploaded_at.localeCompare(left.uploaded_at)),
    })
  }),

  http.post(`${CONFIG}/packs`, async ({ request }) => {
    const url = new URL(request.url)
    const name = url.searchParams.get('name') ?? ''
    if (!isValidPackName(name)) {
      return fail(400, 'validation_failed', 'name must match ^[a-z][a-z0-9_]{0,63}$')
    }

    const body = await request.arrayBuffer()
    const head = new TextDecoder().decode(body.slice(0, 4))
    if (head !== PACK_MAGIC) {
      return fail(400, 'validation_failed', 'not a Godot content pack (missing GDPC header)')
    }

    const sha256 = await sha256OfBytes(body)
    const existing = packs.find((pack) => pack.sha256 === sha256)
    if (existing !== undefined) return HttpResponse.json(existing, { status: 200 })

    const pack: PackState = {
      pack_id: `pack_${String(packs.length + 1).padStart(4, '0')}`,
      name,
      sha256,
      size: body.byteLength,
      uploaded_by: session?.name ?? 'Admin',
      uploaded_at: new Date().toISOString(),
    }
    packs.push(pack)
    return HttpResponse.json(pack, { status: 201 })
  }),

  http.get(`${CONFIG}/namespaces/:namespace/schema`, ({ params }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    return HttpResponse.json({
      namespace: state.name,
      schema_version: state.schema.schema_version,
      schema: state.schema.body,
      created_by: state.schema.created_by,
      created_at: state.schema.created_at,
    })
  }),

  http.put(`${CONFIG}/namespaces/:namespace/schema`, async ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)

    // The write must name the version it edited (If-Match), and a newer
    // one refuses it, as Config does under its row lock.
    const ifMatch = (request.headers.get('If-Match') ?? '').replace(/^"|"$/g, '')
    if (ifMatch === '') {
      return fail(428, 'precondition_required', 'send the schema version you edited (If-Match)')
    }
    if (Number(ifMatch) !== state.schema.schema_version) {
      return fail(
        409,
        'stale_schema',
        `schema v${state.schema.schema_version} was saved since v${ifMatch}`,
      )
    }

    // The body IS the schema document, not an object wrapping it (schemas.go,
    // replaceSchema). A body that is not JSON at all is a 400 before the
    // compiler is reached, exactly as the service reads it.
    let schema: unknown
    try {
      schema = await request.json()
    } catch {
      return fail(400, 'validation_failed', 'schema is not valid JSON')
    }

    const problem = schemaCompileProblem(schema)
    if (problem !== null) {
      return fail(400, 'validation_failed', `schema is not a valid JSON Schema: ${problem}`)
    }

    // A schema is versioned like everything else: replacing it never edits the
    // old one, because a version pins the schema_version it was validated with.
    state.schema = {
      schema_version: state.schema.schema_version + 1,
      body: schema,
      created_by: session?.id ?? 'usr_admin',
      created_at: new Date().toISOString(),
    }
    return HttpResponse.json({
      namespace: state.name,
      schema_version: state.schema.schema_version,
      schema: state.schema.body,
      created_by: state.schema.created_by,
      created_at: state.schema.created_at,
    })
  }),

  http.get(`${CONFIG}/namespaces/:namespace/draft`, ({ params }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    return HttpResponse.json({
      namespace: state.name,
      document: state.draft.document,
      revision: state.draft.revision,
      base_version: state.draft.base_version,
      updated_by: state.draft.updated_by,
      updated_at: state.draft.updated_at,
    })
  }),

  http.put(`${CONFIG}/namespaces/:namespace/draft`, async ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    const body = await readJson<{ document?: unknown; revision?: number }>(request)
    if (body === null || body.document === undefined) {
      return fail(400, 'validation_failed', 'document is required')
    }
    if (typeof body.revision !== 'number') {
      return fail(400, 'validation_failed', 'revision is required')
    }

    // The whole reason this handler set exists: CFG-B3 is `UPDATE ... WHERE
    // revision = $2`, zero rows updated is a 409, and the SPA has to re-read
    // rather than overwrite someone else's work (CFG-D4). Real Config refuses
    // and returns the current draft; the revision is enough here.
    if (body.revision !== state.draft.revision) {
      return fail(409, 'stale_revision', `draft is at revision ${state.draft.revision}`)
    }

    state.draft = {
      document: body.document,
      revision: state.draft.revision + 1,
      base_version: state.draft.base_version,
      updated_by: session?.name ?? 'admin',
      updated_at: new Date().toISOString(),
    }
    return HttpResponse.json({ revision: state.draft.revision, updated_at: state.draft.updated_at })
  }),

  http.post(`${CONFIG}/namespaces/:namespace/draft/validate`, async ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    const body = await readJson<{ document?: unknown }>(request)
    if (body === null || body.document === undefined) {
      return fail(400, 'validation_failed', 'document is required')
    }

    // Validates without saving, and never mutates: the editor calls this on save
    // and the local run is only a hint (CFG-D3, CFG-B4).
    const errors = validateDocument(state.schema.body, body.document)
    return HttpResponse.json({ valid: errors.length === 0, errors })
  }),

  http.get(`${CONFIG}/namespaces/:namespace/versions`, ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)

    const url = new URL(request.url)
    const beforeRaw = url.searchParams.get('before')
    let before: number | null = null
    if (beforeRaw !== null && beforeRaw !== '') {
      const parsed = Number(beforeRaw)
      if (!Number.isInteger(parsed) || parsed < 1) {
        return fail(400, 'validation_failed', 'before must be a positive integer')
      }
      before = parsed
    }
    const limitRaw = url.searchParams.get('limit')
    const limit = limitRaw === null || limitRaw === '' ? 50 : Number(limitRaw)
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      return fail(400, 'validation_failed', 'limit must be an integer between 1 and 200')
    }

    // Newest first, strictly below the cursor when one was given. One extra row
    // would be read to tell a full page from the last; here the length check does
    // the same job without slicing it in.
    const eligible = [...state.versions]
      .reverse()
      .filter((version) => before === null || version.version < before)
    const page = eligible.slice(0, limit)
    const nextBefore = eligible.length > limit ? page[page.length - 1].version : null
    return HttpResponse.json({ versions: page.map(versionRow), next_before: nextBefore })
  }),

  http.get(`${CONFIG}/namespaces/:namespace/versions/:version`, ({ params }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    const wanted = Number(param(params.version))
    const version = state.versions.find((candidate) => candidate.version === wanted)
    if (!version) return fail(404, 'not_found', `unknown version: ${param(params.version)}`)
    const document = serialiseDocument(version.document)
    return HttpResponse.json({
      namespace: state.name,
      ...versionRow(version),
      size: document.length,
      document: version.document,
    })
  }),

  // Cutting a version from the server draft, with the same refusals the store
  // produces: a revision the draft has moved past is a 409 stale_revision, a
  // draft identical to the latest version is a 409 no_changes, and a draft the
  // schema refuses is a 400. On success the draft's base_version moves to the
  // new version and its body and revision are deliberately left alone.
  http.post(`${CONFIG}/namespaces/:namespace/versions`, async ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)

    const body = await readJson<{ message?: unknown; revision?: unknown }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')

    const message = typeof body.message === 'string' ? body.message : ''
    if (message.trim() === '') return fail(400, 'validation_failed', 'message is required')
    if (typeof body.revision !== 'number') {
      return fail(400, 'validation_failed', 'revision is required')
    }
    if (body.revision !== state.draft.revision) {
      return fail(409, 'stale_revision', `the draft has moved on since revision ${body.revision}`)
    }

    const issues = validateDocument(state.schema.body, state.draft.document)
    if (issues.length > 0) {
      const described = issues
        .map((issue) => `${issue.pointer || '/'}: ${issue.message}`)
        .join('; ')
      return fail(
        400,
        'validation_failed',
        `draft does not satisfy schema version ${state.schema.schema_version}: ${described}`,
      )
    }

    const latest = latestVersion(state)
    if (latest && JSON.stringify(state.draft.document) === JSON.stringify(latest.document)) {
      return fail(409, 'no_changes', `the draft is identical to version ${latest.version}`)
    }

    const canonical = serialiseDocument(state.draft.document)
    const created: VersionState = {
      version: (latest?.version ?? 0) + 1,
      schema_version: state.schema.schema_version,
      message: message.trim(),
      created_by: session?.id ?? 'usr_admin',
      created_at: new Date().toISOString(),
      sha256: sha256Hex(canonical),
      document: state.draft.document,
    }
    state.versions.push(created)
    // Versioning does not edit the draft; it only records which version it is
    // based on. The revision stays put, which is why a concurrent bump still
    // conflicts.
    state.draft = { ...state.draft, base_version: created.version }

    return HttpResponse.json(
      { namespace: state.name, ...versionRow(created), size: canonical.length },
      { status: 201 },
    )
  }),

  // A test hook, not a Config endpoint: the e2e conflict flow needs the draft's
  // revision to move under the editor without a second real editor. It is under
  // a `__test__` prefix so it cannot be mistaken for the service's surface.
  http.post(`${CONFIG}/__test__/namespaces/:namespace/draft/bump`, ({ params }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)
    state.draft = {
      ...state.draft,
      revision: state.draft.revision + 1,
      updated_by: 'someone.else@example.com',
      updated_at: new Date().toISOString(),
    }
    return HttpResponse.json({ revision: state.draft.revision })
  }),

  http.get(`${CONFIG}/namespaces/:namespace/diff`, ({ params, request }) => {
    const state = findNamespace(param(params.namespace))
    if (!state) return fail(404, 'not_found', `unknown namespace: ${param(params.namespace)}`)

    const url = new URL(request.url)
    const from = url.searchParams.get('from')
    const to = url.searchParams.get('to')
    if (from === null || from === '' || to === null || to === '') {
      return fail(400, 'validation_failed', 'from and to are required')
    }

    const before = resolveDocument(state, from)
    const after = resolveDocument(state, to)
    if (before === undefined || after === undefined) {
      return fail(404, 'not_found', `cannot diff ${from} against ${to}`)
    }

    return HttpResponse.json({
      from: { ref: from, document: before },
      to: { ref: to, document: after },
      // The walker's result in Config's own `{op, path, from, to}` vocabulary.
      changes: documentDiff(before, after),
    })
  }),

  // A channel's release history. All three channels answer, newest first, and
  // older releases are strictly below `before` (that is what makes `getRelease`'s
  // `before=releaseId+1&limit=1` land on the wanted row). Each row carries its
  // manifest so the diff view and the composer head have something to compare;
  // the real list omits the body, but this fixture's serve both pages.
  http.get(`${CONFIG}/channels/:channel/releases`, ({ params, request }) => {
    const channel = param(params.channel)
    const releases = channelReleases[channel]
    if (releases === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${channel}`)
    }

    const url = new URL(request.url)
    const beforeRaw = url.searchParams.get('before')
    let before: number | null = null
    if (beforeRaw !== null && beforeRaw !== '') {
      const parsed = Number(beforeRaw)
      if (!Number.isInteger(parsed) || parsed < 1) {
        return fail(400, 'validation_failed', 'before must be a positive integer')
      }
      before = parsed
    }

    const limitRaw = url.searchParams.get('limit')
    const limit = limitRaw === null || limitRaw === '' ? 50 : Number(limitRaw)
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      return fail(400, 'validation_failed', 'limit must be an integer between 1 and 200')
    }

    const eligible = releases.filter((release) => before === null || release.release_id < before)
    const page = eligible.slice(0, limit)
    const nextBefore = eligible.length > limit ? page[page.length - 1].release_id : null
    const head = headOf(channel)
    return HttpResponse.json({
      head_release_id: head,
      releases: page.map((release) => ({ ...release, is_head: release.release_id === head })),
      next_before: nextBefore,
    })
  }),

  // Publish, honouring `base_release_id`: a channel that moved past the base is
  // the 409 stale_release the composer's dialog exists for. Both manifests are
  // built from the selections exactly as the store would: client namespaces and
  // packs in one, server namespaces (packs always empty) in the other.
  http.post(`${CONFIG}/channels/:channel/releases`, async ({ params, request }) => {
    const channel = param(params.channel)
    const releases = channelReleases[channel]
    if (releases === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${channel}`)
    }
    // §5 gives publishing to live a higher bar; the gateway enforces it too.
    if (channel === 'live' && !session?.roles.includes('admin')) {
      return fail(403, 'insufficient_role', 'publishing to live requires an admin')
    }

    const body = await readJson<{
      base_release_id?: unknown
      versions?: unknown
      packs?: unknown
      min_client_version?: unknown
      message?: unknown
    }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')

    const base = body.base_release_id
    if (typeof base !== 'number' || !Number.isInteger(base) || base < 1) {
      return fail(400, 'validation_failed', 'base_release_id must be at least 1')
    }
    const minClientVersion =
      typeof body.min_client_version === 'string' ? body.min_client_version : ''
    if (!/^\d+\.\d+\.\d+$/.test(minClientVersion)) {
      return fail(400, 'validation_failed', 'min_client_version must match ^\\d+\\.\\d+\\.\\d+$')
    }
    const message = typeof body.message === 'string' ? body.message : ''
    if (message.trim() === '') return fail(400, 'validation_failed', 'message is required')
    if (!Array.isArray(body.versions)) {
      return fail(400, 'validation_failed', 'versions is required')
    }
    if (!Array.isArray(body.packs)) {
      return fail(400, 'validation_failed', 'packs is required')
    }

    const current = headOf(channel)
    if (current !== base) {
      return fail(
        409,
        'stale_release',
        `channel ${channel} has moved from release ${base} to ${current}`,
      )
    }

    const clientConfig: Record<string, ManifestConfigEntry> = {}
    const serverConfig: Record<string, ManifestConfigEntry> = {}
    const seenNamespaces = new Set<string>()
    for (const rawEntry of body.versions) {
      if (typeof rawEntry !== 'object' || rawEntry === null) {
        return fail(400, 'validation_failed', 'versions entries must name a namespace and version')
      }
      const entry = rawEntry as { namespace?: unknown; version?: unknown }
      const namespace = typeof entry.namespace === 'string' ? entry.namespace : ''
      const version = typeof entry.version === 'number' ? entry.version : Number.NaN
      if (seenNamespaces.has(namespace)) {
        return fail(400, 'validation_failed', `versions contains duplicate namespace ${namespace}`)
      }
      seenNamespaces.add(namespace)
      const state = findNamespace(namespace)
      if (state === undefined) {
        return fail(404, 'not_found', `no such namespace: ${namespace}`)
      }
      const found = state.versions.find((candidate) => candidate.version === version)
      if (found === undefined) {
        return fail(404, 'not_found', `no such version: ${namespace} version ${version}`)
      }
      // The service sorts the one `versions` list by audience; the two manifests
      // are the client and server halves of it.
      const target = state.audience === 'server' ? serverConfig : clientConfig
      target[namespace] = {
        version,
        sha256: found.sha256,
        size: serialiseDocument(found.document).length,
      }
    }

    const selectedPacks: ManifestPackEntry[] = []
    const seenPacks = new Set<string>()
    for (const rawEntry of body.packs) {
      const sha256 =
        typeof rawEntry === 'object' && rawEntry !== null
          ? String((rawEntry as { sha256?: unknown }).sha256 ?? '')
          : ''
      if (!/^[0-9a-f]{64}$/.test(sha256)) {
        return fail(400, 'validation_failed', 'packs sha256 must be 64 lowercase hex characters')
      }
      if (seenPacks.has(sha256)) {
        return fail(400, 'validation_failed', `packs contains duplicate sha256 ${sha256}`)
      }
      seenPacks.add(sha256)
      const found = packs.find((pack) => pack.sha256 === sha256)
      if (found === undefined) return fail(404, 'not_found', `no such pack: ${sha256}`)
      selectedPacks.push({ name: found.name, sha256: found.sha256, size: found.size })
    }
    // §4 fixes the order as sorted by name then sha.
    selectedPacks.sort((left, right) =>
      left.name === right.name
        ? left.sha256.localeCompare(right.sha256)
        : left.name.localeCompare(right.name),
    )

    const releaseId = Math.max(...releases.map((release) => release.release_id)) + 1
    const createdAt = new Date().toISOString()
    const manifest: ManifestState = {
      format: 1,
      channel,
      release_id: releaseId,
      created_at: createdAt,
      min_client_version: minClientVersion,
      config: clientConfig,
      packs: selectedPacks,
    }
    const serverManifest: ManifestState = {
      format: 1,
      channel,
      release_id: releaseId,
      created_at: createdAt,
      min_client_version: minClientVersion,
      config: serverConfig,
      packs: [],
    }
    const release: ReleaseState = {
      release_id: releaseId,
      channel,
      manifest_sha256: sha256Hex(JSON.stringify(manifest)),
      server_manifest_sha256: sha256Hex(JSON.stringify(serverManifest)),
      min_client_version: minClientVersion,
      message: message.trim(),
      created_by: session?.id ?? 'usr_admin',
      created_at: createdAt,
      manifest,
      server_manifest: serverManifest,
    }
    releases.unshift(release)
    channelHead[channel] = releaseId
    return HttpResponse.json(releaseResponse(release), { status: 201 })
  }),

  // A test hook, not a Config endpoint: the e2e stale flow needs the channel head
  // to move under the composer without a second real publish. The new release
  // copies the current head, so it is a legitimate head for the next read.
  http.post(`${CONFIG}/__test__/channels/:channel/head/bump`, ({ params }) => {
    const channel = param(params.channel)
    const releases = channelReleases[channel]
    if (releases === undefined || releases.length === 0) {
      return fail(404, 'not_found', `no such channel: ${channel}`)
    }
    const createdAt = new Date().toISOString()
    const next = cloneRelease(releases[0], channel, releases[0].release_id + 1)
    next.created_at = createdAt
    next.manifest.created_at = createdAt
    next.message = 'someone else published'
    next.manifest_sha256 = sha256Hex(JSON.stringify(next.manifest))
    if (next.server_manifest !== null) {
      next.server_manifest.created_at = createdAt
      next.server_manifest_sha256 = sha256Hex(JSON.stringify(next.server_manifest))
    }
    releases.unshift(next)
    channelHead[channel] = next.release_id
    return HttpResponse.json({ release_id: next.release_id, head_release_id: next.release_id })
  }),

  // Rollback: moves the head pointer back to an earlier release of the same
  // channel without writing a row. `base_release_id` is the head the caller saw;
  // a channel that moved on is the 409 stale_release the dialog exists for, and
  // the current head is the 409 no_changes the view shows inline. Admin only,
  // as the gateway's route table has it.
  http.post(`${CONFIG}/channels/:channel/rollback`, async ({ params, request }) => {
    const channel = param(params.channel)
    const releases = channelReleases[channel]
    if (releases === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${channel}`)
    }
    if (!session?.roles.includes('admin')) {
      return fail(403, 'insufficient_role', 'rolling back requires an admin')
    }

    const body = await readJson<{ release_id?: unknown; base_release_id?: unknown }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')
    const releaseId = body.release_id
    const base = body.base_release_id
    if (typeof releaseId !== 'number' || !Number.isInteger(releaseId) || releaseId < 1) {
      return fail(400, 'validation_failed', 'release_id must be at least 1')
    }
    if (typeof base !== 'number' || !Number.isInteger(base) || base < 1) {
      return fail(400, 'validation_failed', 'base_release_id must be at least 1')
    }

    const current = headOf(channel)
    if (current !== base) {
      return fail(
        409,
        'stale_release',
        `channel ${channel} has moved from release ${base} to ${current}`,
      )
    }
    const target = findRelease(channel, releaseId)
    if (target === undefined) {
      return fail(404, 'not_found', `release ${releaseId} is not a release of channel ${channel}`)
    }
    if (releaseId === current) {
      return fail(409, 'no_changes', 'the channel head is already that release')
    }

    channelHead[channel] = releaseId
    return HttpResponse.json(releaseResponse(target))
  }),

  // Promote: copies the lower channel's head onto this channel as a NEW release.
  // The source is in the query (`?from=`) because the resource promoted into is
  // the path's channel. A target that already carries the source content is the
  // 409 no_changes; a target that moved is 409 stale_release. Admin only.
  http.post(`${CONFIG}/channels/:channel/promote`, async ({ params, request }) => {
    const channel = param(params.channel)
    const releases = channelReleases[channel]
    if (releases === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${channel}`)
    }
    if (!session?.roles.includes('admin')) {
      return fail(403, 'insufficient_role', 'promoting requires an admin')
    }

    const from = new URL(request.url).searchParams.get('from') ?? ''
    const expectedSource = channel === 'staging' ? 'dev' : channel === 'live' ? 'staging' : null
    if (expectedSource === null) {
      return fail(
        400,
        'validation_failed',
        `channel ${channel} has no lower channel to promote from`,
      )
    }
    if (from !== expectedSource) {
      return fail(
        400,
        'validation_failed',
        `from must be ${expectedSource} when promoting to ${channel}`,
      )
    }

    const body = await readJson<{ base_release_id?: unknown; message?: unknown }>(request)
    if (body === null) return fail(400, 'validation_failed', 'request body is not valid JSON')
    const base = body.base_release_id
    if (typeof base !== 'number' || !Number.isInteger(base) || base < 1) {
      return fail(400, 'validation_failed', 'base_release_id must be at least 1')
    }
    const message = typeof body.message === 'string' ? body.message : ''
    if (message.trim() === '') return fail(400, 'validation_failed', 'message is required')

    const current = headOf(channel)
    if (current !== base) {
      return fail(
        409,
        'stale_release',
        `channel ${channel} has moved from release ${base} to ${current}`,
      )
    }
    const sourceHead = findRelease(from, headOf(from))
    if (sourceHead === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${from}`)
    }
    const targetHead = findRelease(channel, current)
    if (targetHead === undefined) {
      return fail(400, 'validation_failed', `no such channel: ${channel}`)
    }
    if (
      targetHead.manifest.min_client_version === sourceHead.manifest.min_client_version &&
      JSON.stringify(targetHead.manifest.config) === JSON.stringify(sourceHead.manifest.config) &&
      JSON.stringify(targetHead.manifest.packs) === JSON.stringify(sourceHead.manifest.packs) &&
      // Like Config's Promote: only the server config counts, and a release from
      // before server manifests existed counts as an empty one.
      JSON.stringify(targetHead.server_manifest?.config ?? {}) ===
        JSON.stringify(sourceHead.server_manifest?.config ?? {})
    ) {
      return fail(409, 'no_changes', `channel ${channel} already carries the content of ${from}`)
    }

    const nextId =
      Math.max(
        ...Object.values(channelReleases)
          .flat()
          .map((release) => release.release_id),
        0,
      ) + 1
    const createdAt = new Date().toISOString()
    const manifest: ManifestState = {
      format: 1,
      channel,
      release_id: nextId,
      created_at: createdAt,
      min_client_version: sourceHead.manifest.min_client_version,
      config: JSON.parse(JSON.stringify(sourceHead.manifest.config)) as Record<
        string,
        ManifestConfigEntry
      >,
      packs: JSON.parse(JSON.stringify(sourceHead.manifest.packs)) as ManifestPackEntry[],
    }
    // A promotion copies the whole snapshot, server half included. Like Config, a
    // new release always gets a server manifest: a source made before server
    // manifests existed promotes with an empty one.
    const serverManifest: ManifestState = {
      format: 1,
      channel,
      release_id: nextId,
      created_at: createdAt,
      min_client_version: sourceHead.manifest.min_client_version,
      config: JSON.parse(JSON.stringify(sourceHead.server_manifest?.config ?? {})) as Record<
        string,
        ManifestConfigEntry
      >,
      packs: [],
    }
    const promoted: ReleaseState = {
      release_id: nextId,
      channel,
      manifest_sha256: sha256Hex(JSON.stringify(manifest)),
      server_manifest_sha256: sha256Hex(JSON.stringify(serverManifest)),
      min_client_version: sourceHead.manifest.min_client_version,
      message: message.trim(),
      created_by: session?.id ?? 'usr_admin',
      created_at: createdAt,
      manifest,
      server_manifest: serverManifest,
    }
    releases.unshift(promoted)
    channelHead[channel] = nextId
    return HttpResponse.json(releaseResponse(promoted), { status: 201 })
  }),

  // Config's own audit, straight from its table: {id, at, actor_id, actor_name,
  // action, target, details}. There is NO `source` field here, because the
  // config.audit_log table in design/02-config.md section 3 has no such column.
  // Dashboard's merged audit adds `source` because it fans out to more than one
  // service (DSH-C11); the two shapes are deliberately different.
  http.get(`${CONFIG}/audit`, ({ request }) => {
    const url = new URL(request.url)
    const page = pageOf(
      configAuditFixture.entries,
      configAuditFixture.page_size,
      url.searchParams.get('cursor'),
    )
    if (page === null) return fail(400, 'validation_failed', 'cursor is not a page cursor')
    return HttpResponse.json(page)
  }),

  // -------------------------------------------------------------------------
  // Dashboard
  // -------------------------------------------------------------------------

  http.get(`${DASHBOARD}/overview`, () => {
    // generated_at is stamped per response: a hard-coded one reads as a dead
    // dashboard a week after the fixture was written.
    //
    // The `otomo.mock.overview` localStorage flag selects a second fixture with
    // ten services, used by the e2e test that asserts all ten cards fit on a
    // 1440x900 viewport. It has to be set before the page boots (Playwright's
    // `addInitScript`); anything other than `many` serves the default.
    const many = safeStorageGet('otomo.mock.overview') === 'many'
    return HttpResponse.json({
      ...(many ? dashboardOverviewManyFixture : dashboardOverviewFixture),
      generated_at: new Date().toISOString(),
    })
  }),

  http.get(`${DASHBOARD}/services`, () => HttpResponse.json(dashboardServicesFixture)),

  http.get(`${DASHBOARD}/services/:name/series`, ({ params, request }) => {
    const service = param(params.name)
    const known = dashboardServicesFixture.services.some((candidate) => candidate.name === service)
    if (!known) return fail(404, 'not_found', `unknown service: ${service}`)

    const url = new URL(request.url)
    const template = url.searchParams.get('metric') ?? ''
    const shape = SERIES_TEMPLATES[template]
    if (!shape) return fail(400, 'validation_failed', `unknown metric template: ${template}`)

    const now = Math.floor(Date.now() / 1000)
    const to = parseTime(url.searchParams.get('to'), now)
    let from = parseTime(url.searchParams.get('from'), to - 3600)
    const step = Math.max(parseTime(url.searchParams.get('step'), 15), 1)
    if (to <= from) return fail(400, 'validation_failed', 'to must be after from')

    // A window wider than the service's maximum is narrowed and reported, which
    // the page turns into "showing the last 7 days".
    const clamped = to - from > MAX_SERIES_SECONDS
    if (clamped) from = to - MAX_SERIES_SECONDS

    // DSH-C6: never more than 1500 points per series. The cap keeps the newest
    // points, so a chart of a long range ends at `to` rather than in the past.
    const total = Math.floor((to - from) / step) + 1
    const count = Math.min(total, MAX_SERIES_POINTS)
    const offset = total - count
    const seed = seedOf(`${service}:${template}`)
    const t: number[] = []
    const series = shape.series.map((name) => ({ name, values: [] as (number | null)[] }))

    for (let index = 0; index < count; index += 1) {
      t.push(from + (offset + index) * step)
      series.forEach((entry, seriesIndex) => {
        // A gap every 97th point, so the null path (spanGaps: false) is real.
        if ((offset + index) % 97 === 0) {
          entry.values.push(null)
          return
        }
        const phase = (index + seed + seriesIndex * 40) / 60
        const value = shape.base + shape.amplitude * (0.5 + 0.5 * Math.sin(phase))
        entry.values.push(round(Math.max(value, 0), shape.decimals))
      })
    }

    return HttpResponse.json({
      metric: template,
      title: shape.title,
      unit: shape.unit,
      service,
      from,
      to,
      step,
      clamped,
      t,
      series,
    })
  }),

  http.get(`${DASHBOARD}/logs`, ({ request }) => {
    const url = new URL(request.url)
    const service = url.searchParams.get('service') ?? ''
    const level = url.searchParams.get('level') ?? ''
    const contains = url.searchParams.get('contains') ?? ''
    const requestId = url.searchParams.get('request_id') ?? ''
    const from = parseTimeMs(url.searchParams.get('from'), 0)
    const to = parseTimeMs(url.searchParams.get('to'), Number.MAX_SAFE_INTEGER)
    // Max 1000 lines (DSH-C8); with `otomo.mock.logs=many` there are ten
    // thousand available, so this is a real cap.
    const limit = Math.min(Number(url.searchParams.get('limit') ?? 1000) || 1000, 1000)

    const entries = logLines()
      .filter((line) => {
        const at = Date.parse(String(line.at))
        if (service !== '' && line.service !== service) return false
        if (level !== '' && line.level !== level) return false
        if (contains !== '' && !String(line.message).includes(contains)) return false
        if (requestId !== '') {
          const fields =
            typeof line.fields === 'object' && line.fields !== null
              ? (line.fields as Record<string, unknown>)
              : null
          if (fields?.request_id !== requestId) return false
        }
        return at >= from && at <= to
      })
      .slice(0, limit)

    return HttpResponse.json({ entries })
  }),

  http.get(`${DASHBOARD}/logs/tail`, async ({ request }) => {
    const url = new URL(request.url)
    const service = url.searchParams.get('service') ?? ''
    const level = url.searchParams.get('level') ?? ''
    const contains = url.searchParams.get('contains') ?? ''
    const rate = tailRate()

    const matches = (line: { service: string; level: string; message: string }): boolean => {
      if (service !== '' && line.service !== service) return false
      if (level !== '' && line.level !== level) return false
      if (contains !== '' && !line.message.includes(contains)) return false
      return true
    }

    // A real stream rather than one JSON body with an event-stream content type:
    // the Logs view has to render lines as they arrive and reconnect when the
    // stream ends, and a single response would test neither. With a rate flag
    // the fixture generates at that rate; without one it replays the finite
    // fixture so the reconnect path stays exercised.
    const encoder = new TextEncoder()
    const stream = new ReadableStream<Uint8Array>({
      async start(controller) {
        try {
          if (rate > 0) {
            await streamGenerated(controller, encoder, rate, matches)
            return
          }
          for (const frame of dashboardLogsFixture.frames) {
            if (matches(frame)) {
              controller.enqueue(encoder.encode(`data: ${JSON.stringify(frame)}\n\n`))
            }
            await delay(dashboardLogsFixture.frame_interval_ms)
          }
          controller.close()
        } catch {
          // The client disconnected mid-tail, which is normal: closing a log
          // view is not an error.
        }
      },
    })

    return new HttpResponse(stream, {
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' },
    })
  }),

  // The merged audit: Config's entries and PHP Admin Auth's interleaved, each
  // carrying the `source` Dashboard added so the UI can say where a row came
  // from. Config's own /audit does not carry that field (see above).
  http.get(`${DASHBOARD}/audit`, ({ request }) => {
    const url = new URL(request.url)
    const source = url.searchParams.get('source') ?? ''
    const actor = url.searchParams.get('actor') ?? ''
    const from = parseTime(url.searchParams.get('from'), 0)
    const to = parseTime(url.searchParams.get('to'), Number.MAX_SAFE_INTEGER)
    // The service caps the page too (DSH-C8); the fixture's own page size is the
    // default so the default request pages at a size a test can walk.
    const limitRaw = url.searchParams.get('limit')
    const limit =
      limitRaw === null || limitRaw === '' ? dashboardAuditFixture.page_size : Number(limitRaw)
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      return fail(400, 'validation_failed', 'limit must be an integer between 1 and 200')
    }

    const entries = dashboardAuditFixture.entries.filter((entry) => {
      const at = Date.parse(entry.at) / 1000
      if (source !== '' && entry.source !== source) return false
      if (actor !== '' && entry.actor_id !== actor && entry.actor_name !== actor) return false
      return at >= from && at <= to
    })

    const page = pageOf(entries, limit, url.searchParams.get('cursor'))
    if (page === null) return fail(400, 'validation_failed', 'cursor is not a page cursor')
    // The merge reports the sources it could not read; the page turns that into
    // a notice rather than pretending the trail is whole.
    return HttpResponse.json({ ...page, degraded: dashboardAuditFixture.degraded })
  }),
]
