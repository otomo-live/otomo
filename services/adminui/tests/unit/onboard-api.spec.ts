import { beforeEach, describe, expect, it } from 'vitest'

import { mfaConfirm, mfaEnroll, onboardLookup, onboardRedeem } from '@/api/auth'
import { ApiError } from '@/api/errors'

/**
 * The onboarding and enrollment client against the fixture API. The fragment
 * handling is `onboard.spec.ts`; this pins the wire contract: lookup fields,
 * each fixture link's outcome, the password policy's refusal, and the fixed
 * enrollment secret/code the e2e suite also uses.
 */

async function failure(promise: Promise<unknown>): Promise<ApiError> {
  const caught = (await promise.catch((error: unknown) => error)) as ApiError
  expect(caught).toBeInstanceOf(ApiError)
  return caught
}

beforeEach(async () => {
  // The fixture remembers the last login, so one test must not decide the next.
  await fetch('/admin-auth/logout', { method: 'POST' })
})

describe('onboardLookup', () => {
  it('returns the invite purpose, the address and the role', async () => {
    const link = await onboardLookup('mock-admin-invite')
    expect(link).toMatchObject({
      purpose: 'invite',
      email: 'new.admin@example.com',
      role: 'admin',
    })
    expect(link.expiresAt).toBeTypeOf('string')
  })

  it('refuses an expired token with invalid_link', async () => {
    const error = await failure(onboardLookup('mock-expired'))
    expect(error.code).toBe('invalid_link')
    expect(error.status).toBe(404)
  })
})

describe('onboardRedeem', () => {
  it('applies the password policy and reports the server message', async () => {
    const error = await failure(onboardRedeem('mock-admin-invite', 'short'))
    expect(error.code).toBe('validation_failed')
    expect(error.message).toContain('12 characters')
  })

  it('sends an admin invite to TOTP enrollment with an enroll ticket', async () => {
    const outcome = await onboardRedeem('mock-admin-invite', 'correct horse battery staple')
    expect(outcome.kind).toBe('mfa_enrollment_required')
  })

  it('signs a live_ops invite straight in', async () => {
    const outcome = await onboardRedeem('mock-liveops-invite', 'correct horse battery staple')
    expect(outcome.kind).toBe('authenticated')
    if (outcome.kind === 'authenticated') {
      expect(outcome.session.user.roles).toEqual(['live_ops'])
    }
  })

  it('asks a TOTP user for their factor after a reset', async () => {
    const outcome = await onboardRedeem('mock-reset-mfa', 'correct horse battery staple')
    expect(outcome.kind).toBe('mfa_required')
  })
})

describe('mfa enrollment', () => {
  it('returns the fixed fixture secret and the otpauth URI', async () => {
    const redeem = await onboardRedeem('mock-admin-invite', 'correct horse battery staple')
    if (redeem.kind !== 'mfa_enrollment_required') throw new Error('expected enrollment')

    const enrollment = await mfaEnroll(redeem.ticket)
    expect(enrollment.secret).toBe('JBSWY3DPEHPK3PXP')
    expect(enrollment.otpauthUrl).toContain('otpauth://totp/')
    expect(enrollment.otpauthUrl).toContain(enrollment.secret)
  })

  it('refuses a wrong code and accepts 123456 with ten recovery codes', async () => {
    const redeem = await onboardRedeem('mock-admin-invite', 'correct horse battery staple')
    if (redeem.kind !== 'mfa_enrollment_required') throw new Error('expected enrollment')

    const wrong = await failure(mfaConfirm(redeem.ticket, '000000'))
    expect(wrong.code).toBe('invalid_code')

    const confirmation = await mfaConfirm(redeem.ticket, '123456')
    expect(confirmation.recoveryCodes).toHaveLength(10)
    expect(confirmation.session?.user.roles).toEqual(['admin'])
  })
})
