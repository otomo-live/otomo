/**
 * The COM-5 error envelope as one type the whole SPA handles.
 *
 * Every 4xx and 5xx from the gateway, Config, Dashboard and PHP Admin Auth
 * carries exactly `{ error: { code, message, request_id } }`. The SPA branches on
 * `code` rather than on the status, because the status alone conflates two
 * refusals that need opposite handling: `expired` says the token is unusable and
 * replacing it may help, while `insufficient_role` says the token is perfectly
 * good and no amount of refreshing can change the answer.
 */

/**
 * The 401 codes that mean the token itself is unusable, so one refresh is worth
 * trying. The first six are the ones the gateway's middleware can produce
 * (services/gateway/internal/authn/middleware.go:22-32); `invalid_credentials`
 * is here because the login and refresh paths use the same envelope for a failed
 * sign-in.
 */
const TOKEN_REPLACEMENT_CODES = new Set([
  'missing_token',
  'invalid_signature',
  'expired',
  'aud_mismatch',
  'iss_mismatch',
  'invalid_token',
  'invalid_credentials',
])

export function isReplaceableTokenError(code: string): boolean {
  return TOKEN_REPLACEMENT_CODES.has(code)
}

export interface ApiErrorInit {
  status: number
  code: string
  message: string
  requestId: string | null
  cause?: unknown
}

/**
 * `code` is a plain string rather than a union of the codes above, which is
 * deliberate: a newer backend may add one, and an unrecognised code has to reach
 * the UI as a generic failure rather than fail to compile or miss a switch arm.
 */
export class ApiError extends Error {
  /** 0 when no response arrived at all: the request never left the browser. */
  readonly status: number
  readonly code: string
  /** COM-5's `request_id`. The one part of a failure worth quoting in a bug report. */
  readonly requestId: string | null

  constructor(init: ApiErrorInit) {
    super(init.message, init.cause === undefined ? undefined : { cause: init.cause })
    this.name = 'ApiError'
    this.status = init.status
    this.code = init.code
    this.requestId = init.requestId
  }
}
