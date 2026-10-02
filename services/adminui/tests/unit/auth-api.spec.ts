import { HttpResponse, http } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { login, logout, mfaEnroll, mfaVerify, refreshSession } from '@/api/auth'
import { resetClient } from '@/api/client'
import { envConfig } from '@/api/env'
import { ApiError } from '@/api/errors'
import { server } from '@/mocks/server'

/**
 * The staff-auth client against the fixture API, through a real `fetch`.
 *
 * The sign-in outcomes are the interesting part: one password form reaches three
 * different branches (a session, a TOTP challenge, a first-factor enrollment),
 * and the client has to tell them apart rather than cast a 200 to a session. The
 * fixture's `denied@` and `mfa.` local parts are what select the branches.
 */

beforeEach(() => {
  resetClient()
})

async function expectApiError(promise: Promise<unknown>, code: string): Promise<ApiError> {
  const error = await promise.then(
    () => null,
    (reason: unknown) => reason,
  )
  expect(error).toBeInstanceOf(ApiError)
  expect((error as ApiError).code).toBe(code)
  return error as ApiError
}

describe('login', () => {
  it('issues a session with the roles the server sent', async () => {
    const outcome = await login('admin@example.com', 'x')

    expect(outcome.kind).toBe('authenticated')
    if (outcome.kind !== 'authenticated') return
    expect(outcome.session.access_token).not.toBe('')
    expect(outcome.session.user.roles).toEqual(['admin'])
  })

  it('returns an MFA challenge, not a session, for an account with a factor', async () => {
    const outcome = await login('mfa.ops@example.com', 'x')

    expect(outcome.kind).toBe('mfa_required')
    if (outcome.kind !== 'mfa_required') return
    expect(outcome.ticket).not.toBe('')
  })

  it('refuses bad credentials in the COM-5 shape', async () => {
    await expectApiError(login('denied@example.com', 'x'), 'invalid_credentials')
  })
})

describe('mfaVerify', () => {
  it('exchanges a challenge ticket and code for a session', async () => {
    const challenge = await login('mfa.ops@example.com', 'x')
    if (challenge.kind !== 'mfa_required') throw new Error('expected an MFA challenge')

    const session = await mfaVerify(challenge.ticket, '123456')

    expect(session.access_token).not.toBe('')
    expect(session.user.roles).toEqual(['admin'])
  })

  it('refuses a ticket the server did not issue', async () => {
    await expectApiError(mfaVerify('not-a-ticket', '123456'), 'validation_failed')
  })
})

describe('mfaEnroll', () => {
  it('refuses an unknown enrollment ticket', async () => {
    await expectApiError(mfaEnroll('not-an-enroll-ticket'), 'invalid_ticket')
  })
})

describe('refreshSession and logout', () => {
  it('refuses a refresh before any sign-in', async () => {
    // The fixture keeps one session for the file; clear whatever an earlier case
    // left so this is genuinely the anonymous path.
    await fetch('/admin-auth/logout', { method: 'POST' })

    await expectApiError(refreshSession(), 'invalid_credentials')
  })

  it('refreshes a live session and then clears it on logout', async () => {
    await login('admin@example.com', 'x')

    const refreshed = await refreshSession()
    expect(refreshed.user.roles).toEqual(['admin'])

    await logout()
    await expectApiError(refreshSession(), 'invalid_credentials')
  })

  it('names a transport failure on logout rather than inventing a status', async () => {
    server.use(http.post(envConfig().auth.logoutPath, () => HttpResponse.error()))

    const error = await expectApiError(logout(), 'network_error')
    expect(error.status).toBe(0)
  })
})
