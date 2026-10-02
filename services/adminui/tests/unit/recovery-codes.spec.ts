import { describe, expect, it } from 'vitest'

import { recoveryCodesFile } from '@/auth/recoveryCodes'

/**
 * The download is the only copy of the recovery codes the server ever gives, so
 * what the file contains is worth pinning: every code, one per line, with enough
 * of a header that a person opening it later knows what it is.
 */

const CODES = [
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

describe('recoveryCodesFile', () => {
  it('writes every code, one per line', () => {
    const text = recoveryCodesFile(CODES)
    const lines = text.split('\n')
    for (const code of CODES) expect(lines).toContain(code)
    expect(text.match(/[A-Z0-9]{4}-[A-Z0-9]{4}/g)).toHaveLength(CODES.length)
  })

  it('explains what the file is and that each code works once', () => {
    const text = recoveryCodesFile(CODES)
    expect(text).toContain('Otomo Admin recovery codes')
    expect(text).toContain('signs you in once')
  })
})
