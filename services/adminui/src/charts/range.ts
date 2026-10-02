/**
 * The time ranges the service detail page offers, and the URL query that
 * carries the selection.
 *
 * Range selection is URL state rather than component state so a link is
 * shareable and a reload reproduces the view. The mapping lives here, apart
 * from the view, so it can be exercised without mounting Ionic.
 *
 * `step` is chosen per range so a request stays in the low hundreds of points.
 * The service caps at 1500 whatever is asked (DSH-C6); going wider than that
 * only asks it to throw points away.
 */

export interface SeriesRange {
  /** The `range` query value, e.g. `6h`. */
  key: string
  /** What the range button reads. */
  label: string
  /** The width of the window in seconds. */
  seconds: number
  /** The resolution of the query in seconds. */
  step: number
}

export const DEFAULT_RANGE_KEY = '1h'

/** Widest range that still polls: 6h and below move fast enough to matter. */
export const MAX_POLLING_SECONDS = 6 * 60 * 60

export const SERIES_RANGES: readonly SeriesRange[] = [
  { key: '15m', label: '15m', seconds: 15 * 60, step: 15 },
  { key: '1h', label: '1h', seconds: 60 * 60, step: 30 },
  { key: '6h', label: '6h', seconds: 6 * 60 * 60, step: 120 },
  { key: '24h', label: '24h', seconds: 24 * 60 * 60, step: 300 },
  { key: '7d', label: '7d', seconds: 7 * 24 * 60 * 60, step: 900 },
]

/** The range a bare or malformed URL resolves to, so a bad link is not empty. */
export const DEFAULT_RANGE: SeriesRange =
  SERIES_RANGES.find((range) => range.key === DEFAULT_RANGE_KEY) ?? SERIES_RANGES[1]

export function rangeByKey(key: string): SeriesRange {
  return SERIES_RANGES.find((range) => range.key === key) ?? DEFAULT_RANGE
}

/** A query value is only accepted when it names a range this page offers. */
export function isRangeKey(value: unknown): value is string {
  return typeof value === 'string' && SERIES_RANGES.some((range) => range.key === value)
}

/** Unknown, missing or repeated values fall back rather than throwing. */
export function parseRangeKey(value: unknown): string {
  if (Array.isArray(value)) return parseRangeKey(value[0])
  return isRangeKey(value) ? value : DEFAULT_RANGE_KEY
}

/** `to` is unix seconds. A non-numeric value is treated as absent, not NaN. */
export function parseTo(value: unknown): number | undefined {
  const raw = Array.isArray(value) ? value[0] : value
  if (typeof raw !== 'string' || raw.trim() === '') return undefined
  const seconds = Number(raw)
  return Number.isFinite(seconds) && seconds > 0 ? Math.floor(seconds) : undefined
}

export interface SeriesWindow {
  from: number
  to: number
  step: number
}

/**
 * The concrete window a range resolves to at `now`. A pinned `to` keeps the
 * window fixed in the past; without one the end is `now`.
 */
export function resolveWindow(range: SeriesRange, now: number, to?: number): SeriesWindow {
  const end = to ?? Math.floor(now / 1000)
  return { from: end - range.seconds, to: end, step: range.step }
}

/** Whether the range is short enough that a 30s poll earns its keep. */
export function isPollingRange(range: SeriesRange): boolean {
  return range.seconds <= MAX_POLLING_SECONDS
}

/**
 * The query a range selection writes to the URL. `to` rides along only when it
 * is pinned, so the ordinary case stays a single shareable `range` value.
 */
export function rangeQuery(rangeKey: string, to?: number): Record<string, string> {
  const query: Record<string, string> = { range: rangeKey }
  if (to !== undefined) query.to = String(to)
  return query
}
