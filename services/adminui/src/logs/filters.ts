/**
 * The log view's filters, and the URL query that carries them.
 *
 * Everything a reader can narrow by lives in the address bar rather than in
 * component state, for the same reason the service detail page's range does: a
 * link that shows a colleague "the errors from gateway_dev in the last fifteen
 * minutes" has to survive being pasted. The parsing and the serialising live
 * here, apart from the view, so both can be exercised without mounting Ionic.
 *
 * The query names match the endpoint's parameters (`request_id`, not
 * `requestId`) so the URL and the request speak the same language.
 */

export const LOG_LEVELS = ['debug', 'info', 'warn', 'error'] as const

export type LogLevel = (typeof LOG_LEVELS)[number]

export interface LogRange {
  key: string
  label: string
  seconds: number
}

export const LOG_RANGES: readonly LogRange[] = [
  { key: '15m', label: 'Last 15 minutes', seconds: 15 * 60 },
  { key: '1h', label: 'Last hour', seconds: 60 * 60 },
  { key: '6h', label: 'Last 6 hours', seconds: 6 * 60 * 60 },
  { key: '24h', label: 'Last 24 hours', seconds: 24 * 60 * 60 },
]

export const CUSTOM_RANGE_KEY = 'custom'
export const DEFAULT_LOG_RANGE_KEY = '1h'

export interface LogFilters {
  service: string
  level: string
  contains: string
  requestId: string
  /** One of `LOG_RANGES[].key`, or `custom`. */
  range: string
  /** `datetime-local` values, only meaningful when `range` is `custom`. */
  from: string
  to: string
}

/** A query value can be a repeated parameter; the first spelling wins. */
function first(value: unknown): string {
  if (Array.isArray(value)) return first(value[0])
  return typeof value === 'string' ? value : ''
}

export function isLogRangeKey(value: unknown): boolean {
  const key = first(value)
  return key === CUSTOM_RANGE_KEY || LOG_RANGES.some((range) => range.key === key)
}

export function isLogLevel(value: unknown): value is LogLevel {
  return (LOG_LEVELS as readonly string[]).includes(first(value))
}

function rangeFor(key: string): LogRange {
  return LOG_RANGES.find((range) => range.key === key) ?? LOG_RANGES[1]
}

/** Unknown, missing or repeated values fall back rather than throwing. */
export function parseLogFilters(query: Record<string, unknown> = {}): LogFilters {
  const rangeKey = first(query.range)
  return {
    service: first(query.service),
    level: isLogLevel(query.level) ? first(query.level) : '',
    contains: first(query.contains),
    requestId: first(query.request_id),
    range: isLogRangeKey(rangeKey) ? rangeKey : DEFAULT_LOG_RANGE_KEY,
    from: first(query.from),
    to: first(query.to),
  }
}

/**
 * The query a filter set writes to the URL. The default range and empty filters
 * are omitted so the common case stays a short, readable link; `from`/`to` ride
 * along only for a custom range.
 */
export function logFiltersQuery(filters: LogFilters): Record<string, string> {
  const query: Record<string, string> = {}
  if (filters.service !== '') query.service = filters.service
  if (filters.level !== '') query.level = filters.level
  if (filters.contains !== '') query.contains = filters.contains
  if (filters.requestId !== '') query.request_id = filters.requestId
  if (filters.range !== DEFAULT_LOG_RANGE_KEY) query.range = filters.range
  if (filters.range === CUSTOM_RANGE_KEY) {
    if (filters.from !== '') query.from = filters.from
    if (filters.to !== '') query.to = filters.to
  }
  return query
}

/** A `datetime-local` value as an ISO instant, or `undefined` when unusable. */
function toIso(value: string): string | undefined {
  if (value === '') return undefined
  const milliseconds = Date.parse(value)
  return Number.isNaN(milliseconds) ? undefined : new Date(milliseconds).toISOString()
}

/**
 * The concrete `from`/`to` the endpoint is asked for. A named range is resolved
 * against `now`; a custom range is passed through as the reader typed it.
 */
export function resolveLogWindow(
  filters: LogFilters,
  nowMilliseconds: number,
): { from?: string; to?: string } {
  if (filters.range === CUSTOM_RANGE_KEY) {
    return { from: toIso(filters.from), to: toIso(filters.to) }
  }
  const range = rangeFor(filters.range)
  return {
    from: new Date(nowMilliseconds - range.seconds * 1000).toISOString(),
    to: new Date(nowMilliseconds).toISOString(),
  }
}

export function logRangeLabel(key: string): string {
  if (key === CUSTOM_RANGE_KEY) return 'Custom range'
  return rangeFor(key).label
}

/**
 * The request_id carried by a line's structured fields, when there is one.
 * Used both to render the follow link and to filter the live tail, which does
 * not take a request id on the server.
 */
export function requestIdOf(line: { fields: Record<string, unknown> | null }): string | null {
  const value = line.fields?.request_id
  return typeof value === 'string' && value !== '' ? value : null
}
