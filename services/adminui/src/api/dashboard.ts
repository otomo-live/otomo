import { request } from '@/api/client'
import {
  arrayIn,
  bool,
  isRecord,
  nullableNum,
  nullableStr,
  num,
  objectIn,
  recordsIn,
  str,
} from '@/api/shape'

/**
 * The Dashboard client: the overview cards, the per-service health list, the log
 * history and tail, and the merged audit log.
 *
 * The paths are design/01-dashboard.md section 4. As with Config, gateway_dev
 * forwards the prefix unstripped, so `/api/admin/dashboard/...` is the whole
 * path.
 *
 * The time series endpoint's one rule is worth writing down, because it is what
 * keeps the browser out of the query language: metrics are named TEMPLATES,
 * never PromQL. The browser sends `metric=p95_ms` and the service maps it to a
 * query.
 */

const BASE = '/api/admin/dashboard'

/**
 * One service's slice of the overview.
 *
 * `up` is "the process answered"; `ready` is "it can serve", which is the
 * distinction the not-ready reason exists for. The three figures are nullable on
 * purpose: a failed Prometheus query is `null` and must stay null rather than
 * becoming a zero the page then paints as a real measurement.
 */
export interface ServiceOverview {
  name: string
  up: boolean
  ready: boolean
  /** The service's own words when it is not ready; empty when it is. */
  reason: string
  rps: number | null
  errorRatio: number | null
  p95Ms: number | null
  version: string
}

export interface HostUsage {
  cpuRatio: number | null
  memRatio: number | null
  diskRatio: number | null
}

/**
 * One firing Prometheus alert, as the Dashboard merges it in. There is no
 * Alertmanager in the stack, so the overview is where staff see these; the
 * service already sorts them critical-first.
 */
export interface OverviewAlert {
  name: string
  /** `"critical"`, `"warning"` or whatever label the rule carried. */
  severity: string
  /** Empty for a cluster-wide alert. */
  service: string
  summary: string
  activeAt: string
}

export interface Overview {
  /** Stamped per response by the service; a fixture's fixed one reads as dead. */
  generatedAt: string
  services: ServiceOverview[]
  host: HostUsage
  onlinePlayers: number | null
  /**
   * The firing alerts, critical first. A missing field is an older Dashboard
   * and reads as "none firing", which is the only safe default: inventing
   * alerts would raise an alarm that was never sent.
   */
  alerts: OverviewAlert[]
  /**
   * The names of the figures that could not be computed, e.g. `["host.disk",
   * "services.p95_ms"]`. A non-empty list is what the page turns into its
   * "some figures are unavailable" notice; it never blanks the page.
   */
  degraded: string[]
}

export interface ScrapeStatus {
  name: string
  up: boolean
  lastScrapeAt: string
  /** Empty when the last scrape succeeded. */
  scrapeError: string
}

export interface LogLine {
  at: string
  service: string
  level: string
  message: string
  /**
   * The structured side of a line: a `request_id` to follow, a route, a
   * duration. `null` rather than `{}` so "the service sent no fields" and "the
   * service sent an empty object" stay distinguishable, even though the view
   * renders neither.
   */
  fields: Record<string, unknown> | null
}

/**
 * A merged audit row: Config's and PHP Admin Auth's interleaved, each carrying
 * the `source` Dashboard added so the UI can say where a row came from. Config's
 * own `/audit` does not carry that field, which is why this client has no shape
 * for it: the shell reads the merged view, and the merge is the service's job.
 */
export interface MergedAuditEntry {
  id: number
  at: string
  actorId: string
  actorName: string
  source: string
  action: string
  target: string
  details: unknown
}

export interface MergedAuditPage {
  entries: MergedAuditEntry[]
  nextCursor: string | null
  /**
   * The sources the merge could not read, e.g. `["admin-auth"]`. A non-empty
   * list is what the page turns into its "the trail is incomplete" notice; the
   * rows that did arrive are still shown.
   */
  degraded: string[]
}

/** Newest first, and capped by the service at 1000 (DSH-C8). */
export interface LogQuery {
  service?: string
  level?: string
  contains?: string
  /** Exact match on a line's `fields.request_id`. */
  requestId?: string
  from?: string
  to?: string
  limit?: number
}

/**
 * One column of a series response. `values` is aligned with `t` and a `null` is
 * a gap the query could not fill: it must stay null rather than becoming a zero
 * the chart would draw as a real measurement.
 */
export interface SeriesColumn {
  name: string
  values: (number | null)[]
}

/**
 * The columnar shape `GET /services/{name}/series` returns (DSH-C6). Time is
 * unix seconds; `clamped` means the service narrowed the window to its maximum
 * and the page has to say so rather than silently drawing a shorter range.
 */
export interface SeriesResult {
  metric: string
  title: string
  unit: string
  service: string
  from: number
  to: number
  step: number
  clamped: boolean
  t: number[]
  series: SeriesColumn[]
}

export interface SeriesQuery {
  from: number
  to: number
  step?: number
}

function queryString(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    // Empty is dropped rather than sent: an empty filter is the absence of a
    // filter, and the service reads `level=` as "no level given" anyway.
    if (value === undefined || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded === '' ? '' : `?${encoded}`
}

function readService(row: Record<string, unknown>): ServiceOverview {
  return {
    name: str(row.name),
    up: bool(row.up),
    ready: bool(row.ready),
    reason: str(row.reason),
    rps: nullableNum(row.rps),
    errorRatio: nullableNum(row.error_ratio),
    p95Ms: nullableNum(row.p95_ms),
    version: str(row.version),
  }
}

export function readLogLine(value: unknown): LogLine {
  const row = isRecord(value) ? value : {}
  return {
    at: str(row.at),
    service: str(row.service),
    level: str(row.level),
    message: str(row.message),
    fields: isRecord(row.fields) ? row.fields : null,
  }
}

function readMergedAudit(row: Record<string, unknown>): MergedAuditEntry {
  return {
    id: num(row.id),
    at: str(row.at),
    actorId: str(row.actor_id),
    actorName: str(row.actor_name),
    source: str(row.source),
    action: str(row.action),
    target: str(row.target),
    details: row.details ?? null,
  }
}

export function readScrapeStatus(row: Record<string, unknown>): ScrapeStatus {
  return {
    name: str(row.name),
    up: bool(row.up),
    lastScrapeAt: str(row.last_scrape_at),
    scrapeError: str(row.scrape_error),
  }
}

function readOverviewAlert(row: Record<string, unknown>): OverviewAlert {
  return {
    name: str(row.name),
    severity: str(row.severity),
    service: str(row.service),
    summary: str(row.summary),
    activeAt: str(row.active_at),
  }
}

export async function getOverview(signal?: AbortSignal): Promise<Overview> {
  const path = `${BASE}/overview`
  const body = objectIn(await request<unknown>(path, { signal }), path)
  const host =
    typeof body.host === 'object' && body.host !== null
      ? (body.host as Record<string, unknown>)
      : {}
  return {
    generatedAt: str(body.generated_at),
    services: recordsIn(body, 'services', path).map(readService),
    host: {
      cpuRatio: nullableNum(host.cpu_ratio),
      memRatio: nullableNum(host.mem_ratio),
      diskRatio: nullableNum(host.disk_ratio),
    },
    onlinePlayers: nullableNum(body.online_players),
    // A missing or non-array `alerts` is an older service and reads as "none
    // firing"; entries that are not objects are dropped rather than rendered
    // as a blank row.
    alerts: Array.isArray(body.alerts) ? body.alerts.filter(isRecord).map(readOverviewAlert) : [],
    // A missing `degraded` is an older service and reads as "nothing degraded",
    // which is the only safe default: inventing names would blank a healthy page.
    degraded: Array.isArray(body.degraded)
      ? body.degraded.filter((name): name is string => typeof name === 'string')
      : [],
  }
}

export async function listServices(): Promise<ScrapeStatus[]> {
  const path = `${BASE}/services`
  return recordsIn(await request<unknown>(path), 'services', path).map(readScrapeStatus)
}

export async function listLogs(query: LogQuery = {}): Promise<LogLine[]> {
  const path = `${BASE}/logs`
  const url = `${path}${queryString({
    service: query.service,
    level: query.level,
    contains: query.contains,
    request_id: query.requestId,
    from: query.from,
    to: query.to,
    limit: query.limit,
  })}`
  return recordsIn(await request<unknown>(url), 'entries', path).map(readLogLine)
}

export async function listMergedAudit(
  query: { source?: string; actor?: string; from?: string; to?: string; cursor?: string } = {},
): Promise<MergedAuditPage> {
  const path = `${BASE}/audit`
  const url = `${path}${queryString({
    source: query.source,
    actor: query.actor,
    from: query.from,
    to: query.to,
    cursor: query.cursor,
  })}`
  const body = objectIn(await request<unknown>(url), path)
  return {
    entries: recordsIn(body, 'entries', path).map(readMergedAudit),
    nextCursor: nullableStr(body.next_cursor),
    // A missing `degraded` is an older service and reads as "nothing degraded",
    // the only safe default: inventing a source would warn about a feed that
    // was read fine.
    degraded: Array.isArray(body.degraded)
      ? body.degraded.filter((name): name is string => typeof name === 'string')
      : [],
  }
}

/**
 * Read a columnar series body. `t` and every `values` column must be arrays:
 * a chart cannot be drawn from a body that lost them, and guessing would put a
 * plausible-looking line on screen from a response that never had one.
 */
export function readSeriesResult(value: unknown, path: string): SeriesResult {
  const body = objectIn(value, path)
  const t = arrayIn(body, 't', path).map((point) => num(point))
  const series = arrayIn(body, 'series', path)
    .filter(isRecord)
    .map((row) => ({
      name: str(row.name),
      // nullableNum keeps a gap a gap; anything non-numeric also reads as one.
      values: arrayIn(row, 'values', path).map(nullableNum),
    }))
  return {
    metric: str(body.metric),
    title: str(body.title),
    unit: str(body.unit),
    service: str(body.service),
    from: num(body.from),
    to: num(body.to),
    step: num(body.step),
    clamped: bool(body.clamped),
    t,
    series,
  }
}

export async function getSeries(
  service: string,
  metric: string,
  query: SeriesQuery,
  signal?: AbortSignal,
): Promise<SeriesResult> {
  const path = `${BASE}/services/${encodeURIComponent(service)}/series`
  const url = `${path}${queryString({
    metric,
    from: query.from,
    to: query.to,
    step: query.step,
  })}`
  return readSeriesResult(await request<unknown>(url, { signal }), path)
}
