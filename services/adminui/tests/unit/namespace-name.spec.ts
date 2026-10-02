import { describe, expect, it } from 'vitest'

import {
  isValidNamespaceName,
  NAMESPACE_NAME_PATTERN,
  namespaceNameProblem,
} from '@/config/namespaceName'

/**
 * The namespace-name rule, against the regexp Config applies in
 * services/config/internal/api/namespaces.go.
 *
 * The cases below are chosen where a hand-written check tends to drift from the
 * server's: segment boundaries, the first character of each segment, and the
 * characters the slug rule excludes.
 */

describe('isValidNamespaceName', () => {
  it('accepts the dot-separated lowercase slug Config accepts', () => {
    for (const name of ['a', 'balance', 'balance.weapons', 'a_b', 'a1.b2_c3', 'x.y.z']) {
      expect(isValidNamespaceName(name)).toBe(true)
    }
  })

  it('rejects a name that is not the slug rule', () => {
    for (const name of [
      '',
      'Balance',
      'balance.Weapons',
      '1balance',
      'balance-weapons',
      'balance..weapons',
      '_balance',
      '.balance',
      'balance.',
      'balance weapons',
      'café',
    ]) {
      expect(isValidNamespaceName(name)).toBe(false)
    }
  })

  it('anchors the pattern, so a valid prefix is not enough', () => {
    expect(NAMESPACE_NAME_PATTERN.test('balance.weapons!')).toBe(false)
    expect(NAMESPACE_NAME_PATTERN.test(' balance.weapons')).toBe(false)
  })
})

describe('namespaceNameProblem', () => {
  it('returns null for a submittable name', () => {
    expect(namespaceNameProblem('balance.weapons')).toBeNull()
  })

  it('names the first problem Config would name', () => {
    expect(namespaceNameProblem('')).toBe('Name is required.')
    expect(namespaceNameProblem('a'.repeat(65))).toBe('Name must be at most 64 characters.')
    expect(namespaceNameProblem('Balance')).toContain('lowercase letters')
  })
})
