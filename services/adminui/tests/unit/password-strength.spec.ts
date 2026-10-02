import { describe, expect, it } from 'vitest'

import { passwordStrength } from '@/auth/passwordStrength'

/**
 * The meter is a hint; the server is the policy. These cases pin that it rewards
 * length and variety, penalises common patterns and the email address, and
 * always returns a label a person can read.
 */

describe('passwordStrength', () => {
  it('calls an empty password very weak', () => {
    const strength = passwordStrength('')
    expect(strength.score).toBe(0)
    expect(strength.label).toBe('Very weak')
  })

  it('rewards length and variety', () => {
    const weak = passwordStrength('short')
    const strong = passwordStrength('Correct-Horse-Battery-9!')
    expect(strong.score).toBeGreaterThan(weak.score)
    expect(strong.label).toBe('Very strong')
  })

  it('penalises a common pattern', () => {
    const common = passwordStrength('password123456')
    expect(common.hints).toContain('Avoid common words and patterns.')
    expect(common.score).toBeLessThanOrEqual(1)
  })

  it('penalises the email address itself', () => {
    const strength = passwordStrength('newadmin-123456', 'newadmin@example.com')
    expect(strength.hints).toContain('Do not use your email address.')
  })

  it('mentions the twelve-character floor before it is met', () => {
    expect(passwordStrength('short').hints).toContain('Use at least 12 characters.')
    expect(passwordStrength('longenough12').hints).not.toContain('Use at least 12 characters.')
  })
})
