import { describe, expect, it } from 'vitest'

import {
  DEFAULT_RANGE_KEY,
  isPollingRange,
  isRangeKey,
  parseRangeKey,
  parseTo,
  rangeByKey,
  rangeQuery,
  resolveWindow,
  SERIES_RANGES,
} from '@/charts/range'

/**
 * Range selection is URL state, so these are the two halves that have to agree:
 * reading a query back into a range, and writing a range back into a query.
 */

describe('parseRangeKey', () => {
  it('accepts the keys the page offers', () => {
    for (const range of SERIES_RANGES) expect(parseRangeKey(range.key)).toBe(range.key)
  })

  it('falls back to the default for a missing, unknown or repeated value', () => {
    expect(parseRangeKey(undefined)).toBe(DEFAULT_RANGE_KEY)
    expect(parseRangeKey('fortnight')).toBe(DEFAULT_RANGE_KEY)
    expect(parseRangeKey(['24h', '7d'])).toBe('24h')
    expect(isRangeKey('6h')).toBe(true)
    expect(isRangeKey('6H')).toBe(false)
    expect(isRangeKey(null)).toBe(false)
  })
})

describe('parseTo', () => {
  it('reads unix seconds', () => {
    expect(parseTo('1700000000')).toBe(1700000000)
  })

  it('treats an absent, empty or nonsense value as unpinned', () => {
    expect(parseTo(undefined)).toBeUndefined()
    expect(parseTo('')).toBeUndefined()
    expect(parseTo('not-a-time')).toBeUndefined()
    expect(parseTo('-5')).toBeUndefined()
  })
})

describe('resolveWindow', () => {
  it('ends at now and spans the range when no end is pinned', () => {
    expect(resolveWindow(rangeByKey('1h'), 1700000000000)).toEqual({
      from: 1699996400,
      to: 1700000000,
      step: 30,
    })
  })

  it('keeps a pinned end fixed in the past', () => {
    expect(resolveWindow(rangeByKey('15m'), 1700000000000, 1699900000)).toEqual({
      from: 1699899100,
      to: 1699900000,
      step: 15,
    })
  })
})

describe('rangeQuery', () => {
  it('writes a single shareable range value', () => {
    expect(rangeQuery('6h')).toEqual({ range: '6h' })
  })

  it('rides a pinned end along', () => {
    expect(rangeQuery('24h', 1700000000)).toEqual({ range: '24h', to: '1700000000' })
  })

  it('round-trips the range through a query', () => {
    for (const range of SERIES_RANGES)
      expect(parseRangeKey(rangeQuery(range.key).range)).toBe(range.key)
  })
})

describe('isPollingRange', () => {
  it('polls 15m, 1h and 6h but not 24h or 7d', () => {
    expect(isPollingRange(rangeByKey('15m'))).toBe(true)
    expect(isPollingRange(rangeByKey('1h'))).toBe(true)
    expect(isPollingRange(rangeByKey('6h'))).toBe(true)
    expect(isPollingRange(rangeByKey('24h'))).toBe(false)
    expect(isPollingRange(rangeByKey('7d'))).toBe(false)
  })
})
