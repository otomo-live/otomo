import { describe, expect, it } from 'vitest'

import { ENROLL_PATH, MFA_PATH } from '@/auth/guard'
import { outcomeDestination } from '@/auth/outcome'

/**
 * One map from an auth outcome to the next screen, shared by the sign-in and
 * onboarding forms. The `returnTo` has to survive the detour through a challenge
 * so a deep link still ends where it was going.
 */

describe('outcomeDestination', () => {
  it('sends an authenticated visitor to the returnTo', () => {
    expect(outcomeDestination('authenticated', '/config')).toEqual({ path: '/config' })
  })

  it('carries the returnTo into the second-factor screen', () => {
    expect(outcomeDestination('mfa_required', '/config')).toEqual({
      path: MFA_PATH,
      query: { returnTo: '/config' },
    })
  })

  it('carries the returnTo into TOTP enrollment', () => {
    expect(outcomeDestination('mfa_enrollment_required', '/dashboard')).toEqual({
      path: ENROLL_PATH,
      query: { returnTo: '/dashboard' },
    })
  })
})
