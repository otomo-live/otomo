import { ENROLL_PATH, MFA_PATH } from '@/auth/guard'
import type { SignInOutcome } from '@/stores/session'

/**
 * Where an auth outcome sends the visitor next.
 *
 * One map, so `LoginView` and `OnboardView` cannot disagree about which
 * challenge screen a `mfa_required` or `mfa_enrollment_required` outcome opens.
 * `authenticated` returns the caller's destination unchanged.
 *
 * The `returnTo` rides along on a challenge so the walk through the factor ends
 * where a plain sign-in would have.
 */
export type AuthDestination = { path: string; query?: { returnTo: string } }

export function outcomeDestination(outcome: SignInOutcome, returnTo: string): AuthDestination {
  if (outcome === 'mfa_required') return { path: MFA_PATH, query: { returnTo } }
  if (outcome === 'mfa_enrollment_required') return { path: ENROLL_PATH, query: { returnTo } }
  return { path: returnTo }
}
