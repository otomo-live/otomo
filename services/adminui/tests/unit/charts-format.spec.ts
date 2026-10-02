import { describe, expect, it } from 'vitest'

import { formatAxis, formatBytes, formatCount, formatValue } from '@/charts/format'

/**
 * The value formatters. A chart's axis and legend are the only place a raw
 * number acquires a unit, so these cases pin the exact strings both draw.
 */

describe('formatBytes', () => {
  it('uses binary units and steps up at 1024', () => {
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1024)).toBe('1.00 KiB')
    expect(formatBytes(1024 * 1024)).toBe('1.00 MiB')
    expect(formatBytes(3 * 1024 * 1024 * 1024)).toBe('3.00 GiB')
  })

  it('keeps a sensible precision across the magnitude', () => {
    expect(formatBytes(1536)).toBe('1.50 KiB')
    expect(formatBytes(20 * 1024)).toBe('20.0 KiB')
    expect(formatBytes(300 * 1024)).toBe('300 KiB')
  })

  it('signs a negative value rather than losing it', () => {
    expect(formatBytes(-2048)).toBe('-2.00 KiB')
  })

  it('formats a non-finite value as an em dash', () => {
    expect(formatBytes(Number.NaN)).toBe('—')
  })
})

describe('formatCount', () => {
  it('groups thousands', () => {
    expect(formatCount(1234567)).toBe('1,234,567')
  })

  it('honours the requested decimals', () => {
    expect(formatCount(1234.5, 1)).toBe('1,234.5')
  })
})

describe('formatValue', () => {
  it('formats each unit the endpoint names', () => {
    expect(formatValue('req/s', 12.34)).toBe('12/s')
    expect(formatValue('req/s', 3.2)).toBe('3.2/s')
    expect(formatValue('%', 0.42)).toBe('0.42%')
    expect(formatValue('ms', 42.6)).toBe('42.6 ms')
    expect(formatValue('s', 0.0042)).toBe('0.004 s')
    expect(formatValue('cores', 0.375)).toBe('0.38 cores')
    expect(formatValue('bytes', 1048576)).toBe('1.00 MiB')
    expect(formatValue('count', 12345)).toBe('12,345')
  })

  it('keeps a gap a gap rather than a zero', () => {
    expect(formatValue('ms', null)).toBe('—')
    expect(formatValue('bytes', undefined)).toBe('—')
    expect(formatValue('count', Number.NaN)).toBe('—')
  })
})

describe('formatAxis', () => {
  it('drops a unit that would repeat on every tick', () => {
    expect(formatAxis('req/s', 12.3)).toBe('12')
    expect(formatAxis('ms', 42.6)).toBe('42.6')
    expect(formatAxis('cores', 0.375)).toBe('0.38')
    expect(formatAxis('s', 0.0042)).toBe('0.004')
  })

  it('keeps the suffix where a bare number would be meaningless', () => {
    expect(formatAxis('%', 0.42)).toBe('0.42%')
    expect(formatAxis('bytes', 1048576)).toBe('1.00 MiB')
    expect(formatAxis('count', 12345)).toBe('12,345')
  })

  it('formats a gap as an empty tick, not an em dash', () => {
    expect(formatAxis('ms', null)).toBe('')
  })
})
