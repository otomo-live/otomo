import { beforeEach, describe, expect, it } from 'vitest'

import {
  changePassword,
  confirmMfa,
  disableMfa,
  enrollMfa,
  fetchAccountStatus,
  listSessions,
  regenerateRecoveryCodes,
  revokeOtherSessions,
} from '@/api/account'
import { ApiError } from '@/api/errors'
import { resetClient } from '@/api/client'

/**
 * The account client against the fixture API, through a real `fetch`.
 *
 * The handlers are stateful (password, sessions, MFA), so these cases assert the
 * state after a call and not just its status: the value of a password change is
 * that the next sign-in needs the new password, and the value of a revoke is that
 * the list is shorter.
 */

const STRONG = 'correct horse battery staple'
const CODE = '123456'

beforeEach(() => {
  resetClient()
})

async function login(email: string, password = 'x'): Promise<Response> {
  return fetch('/admin-auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
}

async function expectApiError(promise: Promise<unknown>, code: string): Promise<void> {
  const error = await promise.then(
    () => null,
    (reason: unknown) => reason,
  )
  expect(error).toBeInstanceOf(ApiError)
  expect((error as ApiError).code).toBe(code)
}

describe('the account password endpoint', () => {
  it('changes the password and makes the next sign-in use it', async () => {
    expect((await login('viewer.password@example.com')).status).toBe(200)

    await changePassword('x', STRONG)

    expect((await login('viewer.password@example.com', 'x')).status).toBe(401)
    expect((await login('viewer.password@example.com', STRONG)).status).toBe(200)
  })

  it('refuses a wrong current password and a password the policy rejects', async () => {
    expect((await login('viewer.policy@example.com')).status).toBe(200)

    await changePassword('x', STRONG)
    await expectApiError(changePassword('not-the-password', STRONG), 'invalid_credentials')
    await expectApiError(changePassword(STRONG, 'short'), 'validation_failed')
  })
})

describe('the account session endpoints', () => {
  it('lists the current session and two others, then revokes the others', async () => {
    expect((await login('liveops.sessions@example.com')).status).toBe(200)

    const before = await listSessions()
    expect(before).toHaveLength(3)
    expect(before.filter((session) => session.current)).toHaveLength(1)

    const revoked = await revokeOtherSessions()
    expect(revoked).toEqual({ revoked: 2, currentKept: true })

    const after = await listSessions()
    expect(after).toHaveLength(1)
    expect(after[0].current).toBe(true)
  })
})

describe('the account status endpoint', () => {
  it('reports is_root and mfa_enabled from the account, and tracks changes', async () => {
    expect((await login('root@example.com')).status).toBe(200)
    expect(await fetchAccountStatus()).toEqual({ isRoot: true, mfaEnabled: true })

    expect((await login('liveops.status1@example.com')).status).toBe(200)
    expect(await fetchAccountStatus()).toEqual({ isRoot: false, mfaEnabled: false })

    await enrollMfa()
    await confirmMfa(CODE)
    expect(await fetchAccountStatus()).toEqual({ isRoot: false, mfaEnabled: true })

    await disableMfa(CODE)
    expect(await fetchAccountStatus()).toEqual({ isRoot: false, mfaEnabled: false })
  })
})

describe('the account MFA endpoints', () => {
  it('regenerates recovery codes only with a valid current code', async () => {
    expect((await login('admin@example.com')).status).toBe(200)

    await expectApiError(regenerateRecoveryCodes('000000'), 'invalid_code')
    const codes = await regenerateRecoveryCodes(CODE)
    expect(codes).toHaveLength(10)
  })

  it('refuses recovery codes for an account with no factor', async () => {
    expect((await login('liveops.nomfa@example.com')).status).toBe(200)
    await expectApiError(regenerateRecoveryCodes(CODE), 'mfa_not_enrolled')
  })

  it('enrolls a non-admin over the bearer, then turns the factor off', async () => {
    expect((await login('liveops.enroll@example.com')).status).toBe(200)

    const secret = await enrollMfa()
    expect(secret.secret).toBe('JBSWY3DPEHPK3PXP')
    expect(secret.otpauthUrl).toContain('otpauth://totp/')

    const codes = await confirmMfa(CODE)
    expect(codes).toHaveLength(10)
    await expectApiError(enrollMfa(), 'mfa_already_enabled')

    await expectApiError(disableMfa('000000'), 'invalid_code')
    await disableMfa(CODE)
    // Off means the next enroll starts a fresh factor rather than 409ing.
    const again = await enrollMfa()
    expect(again.secret).toBe('JBSWY3DPEHPK3PXP')
  })

  it('refuses disable for an admin and for the root account', async () => {
    expect((await login('admin@example.com')).status).toBe(200)
    await expectApiError(disableMfa(CODE), 'mfa_required')

    expect((await login('root@example.com')).status).toBe(200)
    await expectApiError(disableMfa(CODE), 'root_protected')
    await expectApiError(enrollMfa(), 'mfa_not_allowed')
  })
})

describe('the account endpoints with no session', () => {
  it('refuses the status, the session list and the revoke', async () => {
    await fetch('/admin-auth/logout', { method: 'POST' })

    await expectApiError(fetchAccountStatus(), 'invalid_credentials')
    await expectApiError(listSessions(), 'invalid_token')
    await expectApiError(revokeOtherSessions(), 'invalid_token')
  })
})
