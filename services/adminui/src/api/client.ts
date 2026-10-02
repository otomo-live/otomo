import { ApiError, isReplaceableTokenError } from '@/api/errors'

/**
 * The one place the SPA talks HTTP.
 *
 * Everything above it (the auth, Config and Dashboard clients) is a thin wrapper
 * over `request` or `stream`, so that four behaviours exist once rather than per
 * module: the COM-5 error shape, the bearer token, the single-flight refresh
 * after an expiry, and the one retry that follows it.
 *
 * The state here is module-level rather than a store on purpose. The transport
 * has to be reachable from a test with no Pinia installed, and the session store
 * is a USER of this module rather than its owner: it pushes the token in through
 * `setAccessToken` and registers its refresh through `setRefreshHandler`. That
 * direction is what keeps the two from needing each other, which would be a
 * cycle, since the store's own refresh call has to travel through here.
 */

/** Establishes a new session, or throws. The store supplies this; see session.ts. */
export type RefreshHandler = () => Promise<void>

export interface RequestOptions {
  method?: string
  /** Serialised as JSON. `undefined` sends no body at all. */
  body?: unknown
  signal?: AbortSignal
  headers?: Record<string, string>
  /** Never send a bearer token, and never refresh. The auth calls use this. */
  auth?: boolean
  /** Internal: set on the retry, so a refusal cannot loop. */
  retried?: boolean
}

/** The bytes the browser has sent so far, and the size of the body when known. */
export interface UploadProgress {
  loaded: number
  /** `0` when the browser cannot say: `lengthComputable` was false. */
  total: number
}

export interface UploadOptions {
  onProgress?: (progress: UploadProgress) => void
  signal?: AbortSignal
  /** Defaults to the Blob's own type, or `application/octet-stream` when it has none. */
  contentType?: string
  /** Internal: set on the retry, so a refusal cannot loop. */
  retried?: boolean
}

/** A completed upload: XHR exposes the status, which is what tells 201 from 200. */
export interface UploadResult<T> {
  status: number
  data: T
}

/** Empty is the production value: the SPA and its API are the same origin. */
let baseUrl = ''

let accessToken: string | null = null

/**
 * How many tokens have been issued. A request records this when it starts and
 * compares it after a 401. That comparison is what makes "five parallel refusals
 * produce exactly one refresh" true by construction rather than by timing: a
 * request that was refused before a refresh completed retries with the new token
 * instead of starting a refresh of its own.
 */
let tokenGeneration = 0

let refreshHandler: RefreshHandler | null = null
let refreshInFlight: Promise<void> | null = null

export function configureClient(options: { baseUrl: string }): void {
  // A trailing slash would double the one every path already starts with.
  baseUrl = options.baseUrl.replace(/\/+$/, '')
}

export function setAccessToken(token: string | null): void {
  accessToken = token
  // Only a NEW token invalidates a refusal. Clearing the token on sign-out
  // therefore leaves the generation alone, so a request refused before the
  // sign-out is not retried into a second refusal with no credentials at all.
  if (token !== null) tokenGeneration += 1
}

export function setRefreshHandler(handler: RefreshHandler | null): void {
  refreshHandler = handler
}

/**
 * Back to the state a freshly loaded page is in. The tests call this between
 * cases; nothing in the app does, because a sign-out clears the token through
 * `setAccessToken(null)` and leaves the refresh handler registered.
 */
export function resetClient(): void {
  accessToken = null
  tokenGeneration = 0
  refreshHandler = null
  refreshInFlight = null
}

type Outcome =
  { ok: true; status: number; data: unknown } | { ok: false; status: number; error: ApiError }

/** Either the response, or the transport failure that stopped one arriving. */
type Fetched = { ok: true; response: Response } | { ok: false; error: ApiError }

async function fetchOnce(path: string, options: RequestOptions): Promise<Fetched> {
  const headers: Record<string, string> = { Accept: 'application/json', ...options.headers }
  const hasBody = options.body !== undefined
  if (hasBody) headers['Content-Type'] = 'application/json'
  if (options.auth !== false && accessToken !== null) {
    headers.Authorization = `Bearer ${accessToken}`
  }

  try {
    return {
      ok: true,
      response: await fetch(`${baseUrl}${path}`, {
        method: options.method ?? 'GET',
        headers,
        body: hasBody ? JSON.stringify(options.body) : undefined,
        signal: options.signal,
        // The refresh token is an httpOnly cookie, so it rides along or it does
        // not exist. Everything is same-origin here, which is what makes that
        // work; moving the API to another origin means this line AND the
        // gateway's CORS configuration change together.
        credentials: 'same-origin',
      }),
    }
  } catch (cause) {
    // A thrown fetch is a transport failure and not an HTTP one: offline, DNS,
    // or a dev proxy with nothing behind it. It gets the same shape as an HTTP
    // error so a caller only ever handles ApiError, with a code of our own
    // because no envelope arrived to have one.
    return {
      ok: false,
      error: new ApiError({
        status: 0,
        code: 'network_error',
        message: `Could not reach ${path}.`,
        requestId: null,
        cause,
      }),
    }
  }
}

/** A body can be lost mid-stream, so a failure to read one is not a crash. */
async function readText(response: Response): Promise<string> {
  try {
    return await response.text()
  } catch {
    // The status in hand is still worth reporting, which is why this returns
    // empty rather than throwing.
    return ''
  }
}

async function perform(path: string, options: RequestOptions): Promise<Outcome> {
  const fetched = await fetchOnce(path, options)
  if (!fetched.ok) return { ok: false, status: 0, error: fetched.error }

  const { response } = fetched
  const data = parseJson(await readText(response))
  if (response.ok) return { ok: true, status: response.status, data }
  return { ok: false, status: response.status, error: toApiError(response.status, data) }
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    // A body that is not JSON is expected here rather than exceptional: a 502
    // from the gateway when an upstream is not deployed yet, or an HTML error
    // page from whatever sits in front of it.
    return null
  }
}

function toApiError(status: number, body: unknown): ApiError {
  const envelope = isRecord(body) && isRecord(body.error) ? body.error : null
  if (envelope !== null && typeof envelope.code === 'string') {
    return new ApiError({
      status,
      code: envelope.code,
      message: typeof envelope.message === 'string' ? envelope.message : `HTTP ${status}`,
      requestId: typeof envelope.request_id === 'string' ? envelope.request_id : null,
    })
  }

  // No envelope, so the only thing to go on is the status. A 401 or 403 from the
  // gateway itself is still a statement about a token, so it is classified the
  // same way: a 401 is worth one refresh, a 403 is not. Anything else is a
  // failure we cannot name, and says so.
  const code = status === 401 ? 'invalid_token' : status === 403 ? 'insufficient_role' : 'internal'
  return new ApiError({
    status,
    code,
    message: `HTTP ${status} with no COM-5 error body.`,
    requestId: null,
  })
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

/**
 * The session-aware call: bearer token, COM-5 error, and one refresh-and-retry
 * after an expiry.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const generation = tokenGeneration
  const outcome = await perform(path, options)
  if (outcome.ok) return outcome.data as T

  if (await staleToken(outcome.status, outcome.error.code, generation, options.retried === true)) {
    return request<T>(path, { ...options, retried: true })
  }

  throw outcome.error
}

/**
 * The upload twin of `request`: a POST of a Blob, reporting progress, with the
 * same bearer token, COM-5 mapping and one refresh-and-retry after an expiry.
 *
 * XHR rather than `fetch` because only XHR reports how much of a request body
 * has left the browser (`xhr.upload.onprogress`); the upload of a 512 MiB pack
 * needs a progress bar. The port is deliberately narrow: one method, one body
 * kind, and the status handed back because the packs endpoint answers 201 for a
 * new pack and 200 for bytes it already holds.
 */
export async function uploadWithProgress<T>(
  path: string,
  body: Blob,
  options: UploadOptions = {},
): Promise<UploadResult<T>> {
  const generation = tokenGeneration
  const outcome = await performUpload(path, body, options)
  if (outcome.ok) return { status: outcome.status, data: outcome.data as T }

  if (await staleToken(outcome.status, outcome.error.code, generation, options.retried === true)) {
    // The Blob is re-sent: aborted and already-read bodies are the caller's, not
    // ours, and a Blob is safe to hand to XHR more than once.
    return uploadWithProgress<T>(path, body, { ...options, retried: true })
  }

  throw outcome.error
}

async function performUpload(path: string, body: Blob, options: UploadOptions): Promise<Outcome> {
  let response: XhrResponse
  try {
    response = await sendWithXhr(path, body, options)
  } catch (cause) {
    // A cancellation is the caller's own decision, so it must reach them as an
    // AbortError rather than be dressed up as a network failure.
    if (isAbortError(cause)) throw cause
    return {
      ok: false,
      status: 0,
      error: new ApiError({
        status: 0,
        code: 'network_error',
        message: `Could not upload to ${path}.`,
        requestId: null,
        cause,
      }),
    }
  }

  const data = parseJson(response.text)
  if (response.status >= 200 && response.status < 300) {
    return { ok: true, status: response.status, data }
  }
  return { ok: false, status: response.status, error: toApiError(response.status, data) }
}

interface XhrResponse {
  status: number
  text: string
}

function sendWithXhr(path: string, body: Blob, options: UploadOptions): Promise<XhrResponse> {
  return new Promise<XhrResponse>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `${baseUrl}${path}`)
    xhr.responseType = 'text'
    xhr.setRequestHeader('Accept', 'application/json')
    xhr.setRequestHeader(
      'Content-Type',
      options.contentType ?? (body.type === '' ? 'application/octet-stream' : body.type),
    )
    if (accessToken !== null) xhr.setRequestHeader('Authorization', `Bearer ${accessToken}`)

    const signal = options.signal
    const onAbort = (): void => {
      xhr.abort()
      cleanup()
      reject(abortError())
    }
    if (signal !== undefined) {
      if (signal.aborted) {
        reject(abortError())
        return
      }
      signal.addEventListener('abort', onAbort, { once: true })
    }

    const cleanup = (): void => signal?.removeEventListener('abort', onAbort)

    xhr.upload.onprogress = (event: ProgressEvent): void => {
      options.onProgress?.({
        loaded: event.loaded,
        total: event.lengthComputable ? event.total : body.size,
      })
    }

    xhr.onload = (): void => {
      cleanup()
      resolve({ status: xhr.status, text: xhr.responseText })
    }
    xhr.onerror = (): void => {
      cleanup()
      reject(new Error('the upload transport failed'))
    }
    xhr.ontimeout = xhr.onerror
    xhr.onabort = (): void => {
      cleanup()
      reject(abortError())
    }

    try {
      xhr.send(body)
    } catch (cause) {
      cleanup()
      reject(cause)
    }
  })
}

function abortError(): DOMException {
  return new DOMException('The upload was aborted.', 'AbortError')
}

function isAbortError(cause: unknown): boolean {
  return cause instanceof DOMException && cause.name === 'AbortError'
}

/**
 * The same session handling, but handing back the response rather than a parsed
 * body, for a caller that has to read it as it arrives: the log tail.
 *
 * A stream cannot be retried the way a request can. The caller owns the read
 * loop, so it owns the decision to reconnect; what happens here is only the same
 * one refresh, on the same terms, before the stream is opened at all. A stream
 * that fails after it has started is the caller's to retry.
 */
export async function stream(path: string, options: RequestOptions = {}): Promise<Response> {
  const generation = tokenGeneration
  const fetched = await fetchOnce(path, options)
  if (!fetched.ok) throw fetched.error

  const { response } = fetched
  if (response.ok) return response

  const error = toApiError(response.status, parseJson(await readText(response)))
  if (await staleToken(response.status, error.code, generation, options.retried === true)) {
    return stream(path, { ...options, retried: true })
  }
  throw error
}

/**
 * Whether a refusal is one a refresh would fix, and whether the refresh has
 * already happened. Shared so that `request` and `stream` cannot drift on the
 * rules, which are the whole of WEB-4:
 *
 *  - only a 401, never a 403 (`insufficient_role` is the token working);
 *  - only a code that means the token itself is unusable;
 *  - only once, so a refusal cannot loop;
 *  - and only when no other request has already replaced the token while this
 *    one was in flight, which is what collapses five parallel refusals into one
 *    refresh rather than five.
 */
async function staleToken(
  status: number,
  code: string,
  generation: number,
  retried: boolean,
): Promise<boolean> {
  if (status !== 401 || retried || !isReplaceableTokenError(code)) return false
  if (generation === tokenGeneration) await refreshOnce()
  return tokenGeneration !== generation
}

/**
 * The call the auth endpoints are made of: no bearer, and by construction no
 * refresh. A failed sign-in answers 401 `invalid_credentials`, and refreshing on
 * it would be both pointless and circular, since the refresh call is built from
 * this same function.
 */
export async function requestUnauthenticated<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const outcome = await perform(path, { ...options, auth: false })
  if (outcome.ok) return outcome.data as T
  throw outcome.error
}

function refreshOnce(): Promise<void> {
  if (refreshInFlight !== null) return refreshInFlight

  const handler = refreshHandler
  // Nothing can establish a session, so the caller's 401 stands as it is.
  if (handler === null) return Promise.resolve()

  refreshInFlight = handler()
    .catch(() => {
      // A failed refresh is not this layer's error to report. The handler has
      // already cleared the session, and the original refusal is what the caller
      // should see rather than a second-order failure.
    })
    .finally(() => {
      refreshInFlight = null
    })
  return refreshInFlight
}
