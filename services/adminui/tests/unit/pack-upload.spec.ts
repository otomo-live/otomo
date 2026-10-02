import { describe, expect, it } from 'vitest'

import {
  derivePackName,
  formatBytes,
  isPckHeader,
  isValidPackName,
  MAX_PACK_BYTES,
  packSizeProblem,
  uploadResultText,
} from '@/config/packUpload'

/**
 * The client-side pack rules, mirroring Config's `packs.go` so the panel can
 * refuse before spending a slow connection on a file the service would reject in
 * its first four bytes.
 */

const GDPC = Uint8Array.from([0x47, 0x44, 0x50, 0x43])

describe('pack names', () => {
  it('derives a name from the file name, lowercased and folded', () => {
    expect(derivePackName('Weapons-v2.pck')).toBe('weapons_v2')
    expect(derivePackName('My Pack.PCK')).toBe('my_pack')
    expect(derivePackName('audio.pck')).toBe('audio')
  })

  it('truncates a name longer than the grammar allows', () => {
    expect(derivePackName(`${'a'.repeat(80)}.pck`)).toHaveLength(64)
  })

  it('accepts the grammar and refuses everything Config would', () => {
    expect(isValidPackName('a')).toBe(true)
    expect(isValidPackName('a_b9')).toBe(true)
    expect(isValidPackName('a'.repeat(64))).toBe(true)

    expect(isValidPackName('a'.repeat(65))).toBe(false)
    expect(isValidPackName('_leading')).toBe(false)
    expect(isValidPackName('Upper')).toBe(false)
    expect(isValidPackName('has-dash')).toBe(false)
    expect(isValidPackName('')).toBe(false)
  })
})

describe('the GDPC header', () => {
  it('accepts the four magic bytes', () => {
    expect(isPckHeader(GDPC)).toBe(true)
    expect(isPckHeader(Uint8Array.from([...GDPC, 0x00, 0x01]))).toBe(true)
  })

  it('refuses anything else, including a body too short to hold the magic', () => {
    expect(isPckHeader(Uint8Array.from([0x50, 0x4b, 0x03, 0x04]))).toBe(false)
    expect(isPckHeader(Uint8Array.from([0x47, 0x44, 0x50]))).toBe(false)
    expect(isPckHeader(new Uint8Array(0))).toBe(false)
  })
})

describe('the size limit', () => {
  it('allows a file at the limit and names the limit for one over it', () => {
    expect(packSizeProblem(MAX_PACK_BYTES)).toBeNull()
    const problem = packSizeProblem(MAX_PACK_BYTES + 1)
    expect(problem).toContain('512 MiB')
    expect(problem).toContain('limit')
  })
})

describe('formatting', () => {
  it('uses binary units, since the limit is named in MiB', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1024)).toBe('1.00 KiB')
    expect(formatBytes(1048576)).toBe('1.00 MiB')
    expect(formatBytes(MAX_PACK_BYTES)).toBe('512 MiB')
  })

  it('says which of the two upload outcomes happened', () => {
    expect(uploadResultText(true, 'weapons_pack')).toBe('Uploaded')
    expect(uploadResultText(false, 'weapons_pack')).toBe(
      'Already uploaded — same bytes as weapons_pack',
    )
  })
})
