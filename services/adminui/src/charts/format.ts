/**
 * Turning a raw series value into the string a chart axis or legend shows.
 *
 * uPlot has no concept of units, so the unit the series endpoint reports is
 * carried through to here and mapped to a presentation. Keeping it in one
 * module means the axis, the legend and any future tooltip cannot disagree
 * about how a byte or a percentage is written.
 *
 * A `null` is a gap in the data, not a zero: it formats as an em dash and the
 * caller draws nothing at it (`spanGaps: false` in the chart wrapper).
 */

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const

/** `1234567` -> `1,234,567`; used for counts a person reads digit by digit. */
export function formatCount(value: number, decimals = 0): string {
  if (!Number.isFinite(value)) return '—'
  const fixed = value.toFixed(decimals)
  const [whole, fraction] = fixed.split('.')
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return fraction === undefined ? grouped : `${grouped}.${fraction}`
}

/** Binary units, because these are process and host memory figures. */
export function formatBytes(value: number): string {
  if (!Number.isFinite(value)) return '—'
  const sign = value < 0 ? '-' : ''
  let magnitude = Math.abs(value)
  let unit = 0
  while (magnitude >= 1024 && unit < BYTE_UNITS.length - 1) {
    magnitude /= 1024
    unit += 1
  }
  const decimals = unit === 0 ? 0 : magnitude < 10 ? 2 : magnitude < 100 ? 1 : 0
  return `${sign}${magnitude.toFixed(decimals)} ${BYTE_UNITS[unit]}`
}

/** A millisecond figure, kept to a sensible precision for its size. */
function formatMilliseconds(value: number): string {
  if (value !== 0 && Math.abs(value) < 10) return value.toFixed(2)
  if (Math.abs(value) < 100) return value.toFixed(1)
  return value.toFixed(0)
}

function formatSeconds(value: number): string {
  if (value !== 0 && Math.abs(value) < 1) return value.toFixed(3)
  if (Math.abs(value) < 100) return value.toFixed(2)
  return value.toFixed(1)
}

export function formatValue(unit: string, value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  switch (unit) {
    case 'req/s':
      return `${formatCount(value, value < 10 ? 1 : 0)}/s`
    case '%':
      return `${value.toFixed(Math.abs(value) < 1 ? 2 : 1)}%`
    case 'ms':
      return `${formatMilliseconds(value)} ms`
    case 's':
      return `${formatSeconds(value)} s`
    case 'cores':
      return `${value.toFixed(2)} cores`
    case 'bytes':
      return formatBytes(value)
    case 'count':
      return formatCount(value, 0)
    default:
      return formatCount(value, 2)
  }
}

/**
 * The axis variant: tick labels stand alone next to the axis, so the unit is
 * dropped where it would repeat on every one (`/s`, `ms`, `cores`). Percent
 * and byte ticks keep their suffix because `12` or `1.5` alone would be
 * meaningless there.
 */
export function formatAxis(unit: string, value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return ''
  switch (unit) {
    case 'req/s':
      return formatCount(value, value < 10 ? 1 : 0)
    case '%':
      return `${value.toFixed(Math.abs(value) < 1 ? 2 : 0)}%`
    case 'ms':
      return formatMilliseconds(value)
    case 's':
      return formatSeconds(value)
    case 'cores':
      return value.toFixed(2)
    case 'bytes':
      return formatBytes(value)
    case 'count':
      return formatCount(value, 0)
    default:
      return formatCount(value, 2)
  }
}
