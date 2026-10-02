import { HttpResponse, http } from 'msw'
import { describe, expect, it } from 'vitest'

import { listPacks, uploadPack } from '@/api/config'
import { ApiError } from '@/api/errors'
import { server } from '@/mocks/server'

/**
 * The packs endpoints against the fixture API.
 *
 * The list is read through a real `fetch`; the upload is what the SPA really
 * does, a POST of raw bytes through `uploadWithProgress` (XHR). MSW intercepts
 * both in this environment, so the sha256 and the 201/200 dedupe under test are
 * the handler's and not a stub's.
 */

const PACKS = '/api/admin/config/packs'

/** A tiny pack: the GDPC magic plus a tag that makes each test's bytes unique. */
function packBytes(tag: number): ArrayBuffer {
  return Uint8Array.from([0x47, 0x44, 0x50, 0x43, tag]).buffer as ArrayBuffer
}

describe('listPacks', () => {
  it('reads the wire fields into the summary the page renders', async () => {
    const packs = await listPacks()
    const weapons = packs.find((pack) => pack.name === 'weapons_pack')

    expect(weapons).toBeDefined()
    expect(weapons?.sha256).toHaveLength(64)
    expect(weapons?.size).toBe(2621440)
    expect(weapons?.uploadedBy).toBe('liveops@example.com')
    expect(weapons?.uploadedAt).toBe('2026-09-20T11:30:00Z')
    expect(weapons?.packId).toBe('pack_seed_weapons')
  })
})

describe('the POST /packs fixture', () => {
  it('answers 201 with a computed sha for new bytes', async () => {
    const response = await fetch(`${PACKS}?name=unit_api_new`, {
      method: 'POST',
      body: packBytes(3),
    })

    expect(response.status).toBe(201)
    const body = (await response.json()) as { name: string; sha256: string; size: number }
    expect(body.name).toBe('unit_api_new')
    expect(body.sha256).toHaveLength(64)
    expect(body.size).toBe(5)
  })

  it('answers 200 with the first row when the same bytes arrive again', async () => {
    const first = await fetch(`${PACKS}?name=unit_api_dedupe`, {
      method: 'POST',
      body: packBytes(4),
    })
    expect(first.status).toBe(201)

    // A different name, the same bytes: dedupe is by sha256, so the original
    // row comes back rather than a second pack or a conflict.
    const second = await fetch(`${PACKS}?name=unit_api_other_name`, {
      method: 'POST',
      body: packBytes(4),
    })
    expect(second.status).toBe(200)
    const body = (await second.json()) as { name: string }
    expect(body.name).toBe('unit_api_dedupe')
  })

  it('refuses a name outside the grammar and a body without the magic', async () => {
    const badName = await fetch(`${PACKS}?name=Not_Slug`, {
      method: 'POST',
      body: packBytes(5),
    })
    expect(badName.status).toBe(400)

    const badHeader = await fetch(`${PACKS}?name=unit_api_nomagic`, {
      method: 'POST',
      body: Uint8Array.from([0x50, 0x4b, 0x03, 0x04, 0x09]).buffer as ArrayBuffer,
    })
    expect(badHeader.status).toBe(400)
  })
})

describe('uploadPack', () => {
  it('reports created for new bytes and deduped for the same bytes', async () => {
    const file = new Blob([packBytes(9)], { type: 'application/octet-stream' })

    const first = await uploadPack('unit_uploaded', file)
    expect(first.created).toBe(true)
    expect(first.pack.name).toBe('unit_uploaded')
    expect(first.pack.sha256).toHaveLength(64)

    const second = await uploadPack('unit_uploaded_again', file)
    expect(second.created).toBe(false)
    expect(second.pack.sha256).toBe(first.pack.sha256)
  })

  it('surfaces a rejected upload and a failed list as the envelope', async () => {
    const bad = new Blob([Uint8Array.from([0x50, 0x4b, 0x03, 0x04, 0x09])], {
      type: 'application/octet-stream',
    })
    const rejected = await uploadPack('unit_bad', bad).catch((caught: unknown) => caught)
    expect(rejected).toBeInstanceOf(ApiError)
    expect((rejected as ApiError).code).toBe('validation_failed')

    server.use(
      http.get(PACKS, () =>
        HttpResponse.json(
          { error: { code: 'internal', message: 'store unavailable', request_id: 'req_x' } },
          { status: 500 },
        ),
      ),
    )
    const failed = await listPacks().catch((caught: unknown) => caught)
    expect(failed).toBeInstanceOf(ApiError)
    expect((failed as ApiError).status).toBe(500)
  })
})
