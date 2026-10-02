/**
 * The staff role ladder, mirrored from the gateway's own definition.
 *
 * The authority is `services/gateway_dev/internal/authn/claims.go`, whose
 * `roleRank` is `viewer < live_ops < admin` and whose `HasRoleAtLeast` judges a
 * token by the highest role on it. This is deliberately the same shape rather
 * than an independent invention: if the two disagree, the SPA shows a menu item
 * the gateway then refuses, or hides one it would have allowed, and both read as
 * bugs in the other component.
 *
 * It also has to be ordinal rather than a per-role membership test, because the
 * tokens do not arrive with a ladder attached. `admin@example.com` is issued
 * exactly `["admin"]`, not `["admin", "live_ops", "viewer"]`, and `admin` is
 * meant to be able to see the dashboard, whose route asks for `viewer`.
 */

export type StaffRole = 'viewer' | 'live_ops' | 'admin'

const ROLE_RANK: Record<string, number> = {
  viewer: 1,
  live_ops: 2,
  admin: 3,
}

/**
 * The highest rank present, or 0 for a token with nothing recognisable on it.
 *
 * An unknown role ranks 0, which is the same thing the Go side does (`roleRank`
 * misses and yields the zero value) and the safe direction to be wrong in: a
 * role string this build has never heard of must not unlock anything.
 */
export function highestRank(roles: readonly string[]): number {
  let best = 0
  for (const role of roles) {
    const rank = ROLE_RANK[role] ?? 0
    if (rank > best) best = rank
  }
  return best
}

/**
 * Whether these roles meet `required`.
 *
 * Note that this is a UX question and never an authorization one. WEB-5 is
 * explicit that gating in the SPA is presentation: every protected call carries
 * the bearer token and the gateway refuses it on its own, whatever the browser
 * chose to render. Nothing here should ever be the only thing standing between
 * a user and an operation.
 */
export function hasRoleAtLeast(roles: readonly string[], required: string): boolean {
  const min = ROLE_RANK[required] ?? 0
  // A requirement this build cannot rank is not one it can claim to satisfy.
  // Failing closed here means a typo in a route's `minRole` hides the page
  // rather than exposing it.
  if (min === 0) return false
  return highestRank(roles) >= min
}
