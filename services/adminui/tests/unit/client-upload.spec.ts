import { flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  resetClient,
  setAccessToken,
  setRefreshHandler,
  uploadWithProgress,
  type UploadProgress,
} from '@/api/client'
import { ApiError } from '@/api/errors'

/**
 * `uploadWithProgress` against a fake XHR.
 *
 * The real `XMLHttpRequest` cannot be driven deterministically in a unit test
 * (it needs a server and the browser decides when progress fires), so the fake
 * here records the request and lets the test raise progress, a response, an
 * abort or a transport error on cue. Everything the client does with an XHR is
 * therefore observable: the method, the URL, the headers, the retry and the
 * progress callback.
 */

type ProgressHandler = (event: ProgressEvent) => void

class FakeXhr {
  static instances: FakeXhr[] = []

  readonly headers: Record<string, string> = {}
  method = ''
  url = ''
  responseType = ''
  status = 0
  responseText = ''
  sentBody: Blob | null = null
  aborted = false
  upload: { onprogress: ProgressHandler | null } = { onprogress: null }
  onload: (() => void) | null = null
  onerror: (() => void) | null = null
  onabort: (() => void) | null = null
  ontimeout: (() => void) | null = null

  constructor() {
    FakeXhr.instances.push(this)
  }

  open(method: string, url: string): void {
    this.method = method
    this.url = url
  }

  setRequestHeader(name: string, value: string): void {
    this.headers[name] = value
  }

  send(body: Blob): void {
    this.sentBody = body
  }

  abort(): void {
    this.aborted = true
    this.onabort?.()
  }

  emitProgress(loaded: number, total: number): void {
    this.upload.onprogress?.({ loaded, total, lengthComputable: true } as ProgressEvent)
  }

  respond(status: number, text = ''): void {
    this.status = status
    this.responseText = text
    this.onload?.()
  }

  fail(): void {
    this.onerror?.()
  }

  static latest(): FakeXhr {
    const last = FakeXhr.instances[FakeXhr.instances.length - 1]
    if (last === undefined) throw new Error('no XHR was opened')
    return last
  }
}

const PATH = '/api/admin/config/packs?name=weapons_pack'
const BODY = new Blob([Uint8Array.from([0x47, 0x44, 0x50, 0x43])])

function refusal(code: string, message = `refused with ${code}`) {
  return JSON.stringify({ error: { code, message, request_id: 'req_test' } })
}

beforeEach(() => {
  resetClient()
  FakeXhr.instances = []
  vi.stubGlobal('XMLHttpRequest', FakeXhr)
})

afterEach(() => {
  resetClient()
  vi.unstubAllGlobals()
})

describe('uploadWithProgress', () => {
  it('POSTs the Blob and reports progress', async () => {
    const seen: UploadProgress[] = []
    const promise = uploadWithProgress<{ pack_id: string }>(PATH, BODY, {
      contentType: 'application/octet-stream',
      onProgress: (update) => seen.push(update),
    })

    const xhr = FakeXhr.latest()
    xhr.emitProgress(2, 8)
    xhr.emitProgress(8, 8)
    xhr.respond(201, JSON.stringify({ pack_id: 'pack_new' }))

    const result = await promise
    expect(xhr.method).toBe('POST')
    expect(xhr.url).toBe(PATH)
    expect(xhr.sentBody).toBe(BODY)
    expect(xhr.headers['Content-Type']).toBe('application/octet-stream')
    expect(seen).toEqual([
      { loaded: 2, total: 8 },
      { loaded: 8, total: 8 },
    ])
    expect(result).toEqual({ status: 201, data: { pack_id: 'pack_new' } })
  })

  it('sends the bearer token when one is set', async () => {
    setAccessToken('tok_upload')
    const promise = uploadWithProgress(PATH, BODY)
    const xhr = FakeXhr.latest()
    expect(xhr.headers.Authorization).toBe('Bearer tok_upload')
    xhr.respond(201, '{}')
    await promise
  })

  it('is abortable, and rejects with an AbortError', async () => {
    const controller = new AbortController()
    const promise = uploadWithProgress(PATH, BODY, { signal: controller.signal })
    const xhr = FakeXhr.latest()

    controller.abort()

    await expect(promise).rejects.toMatchObject({ name: 'AbortError' })
    expect(xhr.aborted).toBe(true)
  })

  it('maps a COM-5 refusal onto an ApiError', async () => {
    const promise = uploadWithProgress(PATH, BODY)
    FakeXhr.latest().respond(
      400,
      JSON.stringify({
        error: {
          code: 'validation_failed',
          message: 'not a Godot content pack (missing GDPC header)',
          request_id: 'req_400',
        },
      }),
    )

    const error = (await promise.catch((caught: unknown) => caught)) as ApiError
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(400)
    expect(error.code).toBe('validation_failed')
    expect(error.message).toContain('GDPC')
    expect(error.requestId).toBe('req_400')
  })

  it('names a transport failure rather than inventing an HTTP status', async () => {
    const promise = uploadWithProgress(PATH, BODY)
    FakeXhr.latest().fail()

    const error = (await promise.catch((caught: unknown) => caught)) as ApiError
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(0)
    expect(error.code).toBe('network_error')
  })

  it('refreshes once and re-sends the same Blob after an expiry', async () => {
    let refreshes = 0
    setAccessToken('tok_stale')
    setRefreshHandler(async () => {
      refreshes += 1
      setAccessToken('tok_fresh')
    })

    const promise = uploadWithProgress(PATH, BODY)
    const first = FakeXhr.latest()
    first.respond(401, refusal('expired'))
    await flushPromises()

    expect(refreshes).toBe(1)
    const second = FakeXhr.latest()
    expect(second).not.toBe(first)
    expect(second.headers.Authorization).toBe('Bearer tok_fresh')
    expect(second.sentBody).toBe(BODY)

    second.respond(200, JSON.stringify({ pack_id: 'pack_existing' }))
    const result = await promise
    expect(result.status).toBe(200)
    expect(result.data).toEqual({ pack_id: 'pack_existing' })
  })

  it('does not loop when the refreshed token is refused too', async () => {
    let refreshes = 0
    setAccessToken('tok_stale')
    setRefreshHandler(async () => {
      refreshes += 1
      setAccessToken(`tok_${refreshes}`)
    })

    const promise = uploadWithProgress(PATH, BODY)
    FakeXhr.latest().respond(401, refusal('expired'))
    await flushPromises()
    FakeXhr.latest().respond(401, refusal('expired'))

    const error = (await promise.catch((caught: unknown) => caught)) as ApiError
    expect(error.code).toBe('expired')
    expect(refreshes).toBe(1)
    expect(FakeXhr.instances).toHaveLength(2)
  })
})
