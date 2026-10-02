import { describe, expect, it } from 'vitest'

import { hasRoleAtLeast, highestRank } from '@/auth/roles'

/**
 * The ladder, which the SPA and `gateway_dev/internal/authn/claims.go` have to
 * agree on. The cases that matter are not the happy ones but the two edges: a
 * token carrying only the top role, and a role string neither side knows.
 */

describe('the staff role ladder', () => {
  it('ranks viewer < live_ops < admin', () => {
    expect(highestRank(['viewer'])).toBeLessThan(highestRank(['live_ops']))
    expect(highestRank(['live_ops'])).toBeLessThan(highestRank(['admin']))
  })

  it('judges a multi-role token by its highest role', () => {
    expect(hasRoleAtLeast(['viewer', 'live_ops'], 'live_ops')).toBe(true)
    expect(hasRoleAtLeast(['live_ops', 'viewer'], 'admin')).toBe(false)
  })

  it('lets an admin-only token reach a viewer route', () => {
    // The reason the ladder is ordinal: the server issues ["admin"], not the
    // transitive closure. A membership test would lock the administrator out of
    // the dashboard.
    expect(hasRoleAtLeast(['admin'], 'viewer')).toBe(true)
    expect(hasRoleAtLeast(['admin'], 'live_ops')).toBe(true)
    expect(hasRoleAtLeast(['admin'], 'admin')).toBe(true)
  })

  it('grants nothing to a role this build has never heard of', () => {
    expect(highestRank(['auditor'])).toBe(0)
    expect(hasRoleAtLeast(['auditor'], 'viewer')).toBe(false)
    // Including when it arrives alongside a role that is known: the highest one
    // still decides, and the unknown one is not a bonus.
    expect(hasRoleAtLeast(['auditor', 'viewer'], 'live_ops')).toBe(false)
  })

  it('grants nothing to a token with no roles at all', () => {
    expect(highestRank([])).toBe(0)
    expect(hasRoleAtLeast([], 'viewer')).toBe(false)
  })

  it('cannot satisfy a requirement it cannot rank', () => {
    // Otherwise a typo in a route's minRole would expose the page rather than
    // hide it.
    expect(hasRoleAtLeast(['admin'], 'superuser')).toBe(false)
  })
})
