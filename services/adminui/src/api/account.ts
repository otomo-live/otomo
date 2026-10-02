import { request } from '@/api/client'
import { envConfig } from '@/api/env'
import { ApiError } from '@/api/errors'
import { arrayIn, bool, num, objectIn, recordsIn, str } from '@/api/shape'

/**
 * The account client, coded against `admin_auth`'s account routes.
 *
 * Every call here is made with the bearer token the API client already holds, so
 * there is no `requestUnauthenticated` in this file: a signed-in person is the
 * only caller. The routes live under `/admin-auth/account/*` and are proxied
 * unstripped by the gateway, the same way the staff session routes are.
 *
 * MFA has two shapes on this page and only one of them is new. Enabling a factor
 * happens through the existing `/admin-auth/mfa/enroll` and `/mfa/confirm`
 * endpoints, in their **bearer** mode: no ticket, just the session. What the
 * account page adds is the ability to replace recovery codes and to turn a
 * non-mandatory factor off.
 *
 * Whether an account already has a factor, and whether it is the password-only
 * root account, both come from `/admin-auth/me`, which answers `{id, name, roles,
 * is_root, mfa_enabled}` from the caller's row. Enrolling is only ever started by an explicit "Set up",
 * because it writes a pending secret.
 */

const BASE = '/admin-auth/account'

export interface AccountSession {
  id: string
  createdAt: string
  lastUsedAt: string
  ip: string
  userAgent: string
  /** True for the session that made this request. */
  current: boolean
}

export interface RevokeOthersResult {
  revoked: number
  currentKept: boolean
}

/** A pending TOTP secret, in the same shape the enrollment view receives. */
export interface AccountEnrollment {
  secret: string
  otpauthUrl: string
}

/** The presentation-relevant facts from `/admin-auth/me`. */
export interface AccountStatus {
  isRoot: boolean
  mfaEnabled: boolean
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === 'string')
}

/**
 * Reads the signed-in account's status. The server answers from the caller's DB
 * row, so this is the one call that tells the page both whether a factor exists
 * and whether the account may have one, without enrolling.
 */
export async function fetchAccountStatus(): Promise<AccountStatus> {
  const path = envConfig().auth.mePath
  const body = objectIn(await request<unknown>(path), path)
  return { isRoot: bool(body.is_root), mfaEnabled: bool(body.mfa_enabled) }
}

/**
 * Changes the password of the signed-in account. The server revokes every other
 * refresh family, which is why the page refreshes its session list afterwards.
 */
export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  await request<void>(`${BASE}/password`, {
    method: 'POST',
    body: { current_password: currentPassword, new_password: newPassword },
  })
}

/** The signed-in account's live sessions, current one included. */
export async function listSessions(): Promise<AccountSession[]> {
  const path = `${BASE}/sessions`
  return recordsIn(await request<unknown>(path), 'sessions', path).map((row) => ({
    id: str(row.id),
    createdAt: str(row.created_at),
    lastUsedAt: str(row.last_used_at),
    ip: str(row.ip),
    userAgent: str(row.user_agent),
    current: bool(row.current),
  }))
}

/** Revokes every session except the caller's, and reports how many went. */
export async function revokeOtherSessions(): Promise<RevokeOthersResult> {
  const path = `${BASE}/sessions/revoke-others`
  const body = objectIn(await request<unknown>(path, { method: 'POST' }), path)
  return { revoked: num(body.revoked), currentKept: bool(body.current_kept) }
}

/**
 * Replaces the recovery codes. Requires a current TOTP or recovery code; an
 * account with no factor is `409 mfa_not_enrolled`.
 */
export async function regenerateRecoveryCodes(code: string): Promise<string[]> {
  const path = `${BASE}/mfa/recovery-codes`
  const body = objectIn(await request<unknown>(path, { method: 'POST', body: { code } }), path)
  const codes = arrayIn(body, 'recovery_codes', path)
  if (!isStringArray(codes)) {
    throw new ApiError({
      status: 200,
      code: 'internal',
      message: `${path} answered with a recovery_codes array that is not all strings.`,
      requestId: null,
    })
  }
  return codes
}

/**
 * Turns the factor off. Only a non-admin may call it: an admin is answered `403
 * mfa_required` and the root account `403 root_protected`.
 */
export async function disableMfa(code: string): Promise<void> {
  await request<void>(`${BASE}/mfa/disable`, { method: 'POST', body: { code } })
}

/** Starts enrollment for a signed-in account: a fresh secret, no ticket. */
export async function enrollMfa(): Promise<AccountEnrollment> {
  const path = envConfig().auth.mfaEnrollPath
  const body = objectIn(await request<unknown>(path, { method: 'POST', body: {} }), path)
  const secret = str(body.secret)
  const otpauthUrl = str(body.otpauth_url)
  if (secret === '' || otpauthUrl === '') {
    throw new ApiError({
      status: 200,
      code: 'internal',
      message: `${path} answered without a secret and an otpauth_url.`,
      requestId: null,
    })
  }
  return { secret, otpauthUrl }
}

/** Confirms the pending factor and returns the ten recovery codes. */
export async function confirmMfa(code: string): Promise<string[]> {
  const path = envConfig().auth.mfaConfirmPath
  const body = objectIn(await request<unknown>(path, { method: 'POST', body: { code } }), path)
  const codes = arrayIn(body, 'recovery_codes', path)
  if (!isStringArray(codes)) {
    throw new ApiError({
      status: 200,
      code: 'internal',
      message: `${path} answered with a recovery_codes array that is not all strings.`,
      requestId: null,
    })
  }
  return codes
}
