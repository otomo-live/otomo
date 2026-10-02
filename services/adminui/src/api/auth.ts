import { requestUnauthenticated } from '@/api/client'
import { envConfig } from '@/api/env'
import { ApiError } from '@/api/errors'

/**
 * The staff auth client, coded against the shapes this branch proposes in
 * design/06-auth-identity-contract.md. PHP Admin Auth is not in this repository, so
 * these are a contract the SPA holds up rather than a transcription of something
 * that exists: if the real endpoints disagree, this file changes and nothing
 * else does.
 *
 * Two properties are load bearing:
 *
 *  - Both `login` and `refresh` return `user`, roles included, so the SPA never
 *    decodes the token to read a claim. Decoding an unverified JWT to display a
 *    role is the shortcut that quietly becomes an authorization check.
 *  - The access token stays opaque. Nothing here inspects it, not even to read
 *    `exp`.
 *
 * The responses are checked rather than cast. A 200 that is not one of the two
 * shapes is a proxy or a captive portal answering for the real service, and a
 * cast would turn that into a session whose fields are `undefined`.
 *
 * It also has the onboarding pair and TOTP enrollment. `login` and
 * `onboardRedeem` answer the same three-way outcome, which is why both return
 * `AuthOutcome`: a password can be accepted and still need a second factor
 * (`mfa_required`) or a first one (`mfa_enrollment_required`, an admin with no
 * confirmed TOTP). The ticket that carries the challenge lives in memory in the
 * session store, never in the address bar.
 */

export interface SessionUser {
  id: string
  name: string
  /** Exactly as the server sends them: `viewer`, `live_ops`, `admin`. */
  roles: string[]
}

export interface IssuedSession {
  access_token: string
  /** Seconds. Converted to an absolute time by the store. */
  expires_in: number
  user: SessionUser
}

export interface MfaChallenge {
  mfa_required?: true
  mfa_enrollment_required?: true
  mfa_ticket: string
}

/** `login`, `onboardRedeem` and `mfaVerify`'s success are all this. */
export type AuthOutcome =
  | { kind: 'authenticated'; session: IssuedSession }
  | { kind: 'mfa_required'; ticket: string }
  | { kind: 'mfa_enrollment_required'; ticket: string }

/** Kept as its own name because the sign-in form and the onboarding page share it. */
export type LoginOutcome = AuthOutcome

/** What `lookup` says a link is for. `role` is null on a reset link. */
export interface OnboardLink {
  purpose: 'invite' | 'reset'
  email: string
  name: string
  role: string | null
  expiresAt: string
}

/**
 * A pending TOTP secret. `otpauth_url` is the enrolment URI the QR encodes;
 * `secret` is the same secret in base32 for manual entry.
 */
export interface EnrollmentSecret {
  secret: string
  otpauthUrl: string
}

export interface EnrollmentConfirmation {
  recoveryCodes: string[]
  /**
   * Present when confirm was made with a ticket, which is the SPA's only mode: an
   * onboarding or login challenge ends signed in. Null is kept for the bearer
   * form the account page may use later.
   */
  session: IssuedSession | null
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function isSessionUser(value: unknown): value is SessionUser {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.name === 'string' &&
    Array.isArray(value.roles) &&
    value.roles.every((role) => typeof role === 'string')
  )
}

function isIssuedSession(value: unknown): value is IssuedSession {
  return (
    isRecord(value) &&
    typeof value.access_token === 'string' &&
    value.access_token.length > 0 &&
    typeof value.expires_in === 'number' &&
    isSessionUser(value.user)
  )
}

function isMfaChallenge(value: unknown): value is MfaChallenge {
  return (
    isRecord(value) &&
    typeof value.mfa_ticket === 'string' &&
    (value.mfa_required === true || value.mfa_enrollment_required === true)
  )
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === 'string')
}

/** The status is the one in hand: a 200 that is neither shape is a server bug. */
function unexpected(status: number, path: string): ApiError {
  return new ApiError({
    status,
    code: 'internal',
    message: `${path} answered with an unexpected body.`,
    requestId: null,
  })
}

function asIssuedSession(body: unknown, path: string): IssuedSession {
  if (!isIssuedSession(body)) throw unexpected(200, path)
  return body
}

/** Every endpoint that ends in a session or a challenge reads its body this way. */
function asAuthOutcome(body: unknown, path: string): AuthOutcome {
  if (isMfaChallenge(body)) {
    if (body.mfa_enrollment_required === true) {
      return { kind: 'mfa_enrollment_required', ticket: body.mfa_ticket }
    }
    return { kind: 'mfa_required', ticket: body.mfa_ticket }
  }
  return { kind: 'authenticated', session: asIssuedSession(body, path) }
}

export async function login(email: string, password: string): Promise<LoginOutcome> {
  const path = envConfig().auth.loginPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { email, password },
  })
  return asAuthOutcome(body, path)
}

/**
 * Tells the page whose link this is. The token is posted in the body, never in
 * the URL, so it cannot reach a proxy log or a Referer header.
 */
export async function onboardLookup(token: string): Promise<OnboardLink> {
  const path = envConfig().auth.onboardLookupPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { token },
  })

  if (
    !isRecord(body) ||
    (body.purpose !== 'invite' && body.purpose !== 'reset') ||
    typeof body.email !== 'string' ||
    typeof body.name !== 'string' ||
    typeof body.expires_at !== 'string'
  ) {
    throw unexpected(200, path)
  }

  return {
    purpose: body.purpose,
    email: body.email,
    name: body.name,
    role: typeof body.role === 'string' ? body.role : null,
    expiresAt: body.expires_at,
  }
}

/** Sets the password and consumes the link. The outcome is login's, exactly. */
export async function onboardRedeem(token: string, password: string): Promise<AuthOutcome> {
  const path = envConfig().auth.onboardRedeemPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { token, password },
  })
  return asAuthOutcome(body, path)
}

/** Starts enrollment: a fresh secret and the otpauth URI the QR encodes. */
export async function mfaEnroll(ticket: string): Promise<EnrollmentSecret> {
  const path = envConfig().auth.mfaEnrollPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { mfa_ticket: ticket },
  })

  if (!isRecord(body) || typeof body.secret !== 'string' || typeof body.otpauth_url !== 'string') {
    throw unexpected(200, path)
  }
  return { secret: body.secret, otpauthUrl: body.otpauth_url }
}

/**
 * Proves the first TOTP and receives the ten recovery codes. With a ticket the
 * body also carries the login success fields, so this is where enrollment adopts
 * the session.
 */
export async function mfaConfirm(ticket: string, code: string): Promise<EnrollmentConfirmation> {
  const path = envConfig().auth.mfaConfirmPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { mfa_ticket: ticket, code },
  })

  if (!isRecord(body) || !isStringArray(body.recovery_codes)) throw unexpected(200, path)
  return {
    recoveryCodes: body.recovery_codes,
    session: isIssuedSession(body) ? body : null,
  }
}

export async function mfaVerify(ticket: string, code: string): Promise<IssuedSession> {
  const path = envConfig().auth.mfaPath
  const body = await requestUnauthenticated<unknown>(path, {
    method: 'POST',
    body: { mfa_ticket: ticket, code },
  })
  return asIssuedSession(body, path)
}

/**
 * Redeems the refresh cookie for a new access token. No bearer is sent, and none
 * should be: this is the call that exists for when the current one is no good.
 */
export async function refreshSession(): Promise<IssuedSession> {
  const path = envConfig().auth.refreshPath
  const body = await requestUnauthenticated<unknown>(path, { method: 'POST' })
  return asIssuedSession(body, path)
}

/**
 * Clears the cookie server-side. Sent without a bearer for the same reason as
 * `refreshSession`: signing out with an expired token is still signing out.
 */
export async function logout(): Promise<void> {
  await requestUnauthenticated<void>(envConfig().auth.logoutPath, { method: 'POST' })
}
