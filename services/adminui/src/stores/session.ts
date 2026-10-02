import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import type { AuthOutcome, EnrollmentSecret, IssuedSession, SessionUser } from '@/api/auth'
import {
  login as loginRequest,
  logout as logoutRequest,
  mfaConfirm,
  mfaEnroll,
  mfaVerify,
  onboardRedeem as onboardRedeemRequest,
  refreshSession,
} from '@/api/auth'
import { setAccessToken, setRefreshHandler } from '@/api/client'

export type SessionStatus = 'unknown' | 'anonymous' | 'authenticated'

/** Where a sign-in or onboarding step left the caller. */
export type SignInOutcome = 'authenticated' | 'mfa_required' | 'mfa_enrollment_required'

/** What a held `mfaTicket` is for: proving a factor, or setting one up. */
export type MfaPurpose = 'verify' | 'enroll'

/**
 * How long a challenge ticket is good for. The server's ticket is authoritative
 * (five minutes); this copy only drives the enrollment screen's
 * countdown, so receiving one a little late makes it generous rather than wrong.
 */
const MFA_TICKET_TTL_MS = 5 * 60 * 1000

/**
 * The staff session, in memory only (WEB-4).
 *
 * The access token lives in a ref and is pushed into the API client, which is
 * the only thing that puts it on a request. Nothing here writes it to
 * localStorage or sessionStorage: web storage is readable by any script on the
 * origin, and the httpOnly refresh cookie is what makes a reload survive
 * instead.
 *
 * `status` starts at 'unknown' rather than 'anonymous' so a route guard can tell
 * "not signed in" from "have not asked yet". Collapsing the two renders the login
 * screen for one frame on every reload, just before a restored session replaces
 * it, which reads as being thrown out and let back in.
 */
export const useSessionStore = defineStore('session', () => {
  const accessToken = ref<string | null>(null)
  const user = ref<SessionUser | null>(null)
  const expiresAt = ref<number | null>(null)
  const status = ref<SessionStatus>('unknown')
  const mfaTicket = ref<string | null>(null)
  const mfaPurpose = ref<MfaPurpose | null>(null)
  const mfaTicketExpiresAt = ref<number | null>(null)
  const email = ref<string | null>(null)

  const roles = computed(() => user.value?.roles ?? [])
  const displayName = computed(() => user.value?.name ?? '')

  function adopt(issued: IssuedSession): void {
    accessToken.value = issued.access_token
    user.value = issued.user
    expiresAt.value = Date.now() + issued.expires_in * 1000
    status.value = 'authenticated'
    mfaTicket.value = null
    mfaPurpose.value = null
    mfaTicketExpiresAt.value = null
    setAccessToken(issued.access_token)
  }

  function clear(): void {
    accessToken.value = null
    user.value = null
    expiresAt.value = null
    status.value = 'anonymous'
    mfaTicket.value = null
    mfaPurpose.value = null
    mfaTicketExpiresAt.value = null
    setAccessToken(null)
  }

  /**
   * Records what a challenge response means: a session, a factor to prove, or a
   * factor to set up. The last two leave `status` anonymous, because no session
   * exists yet and a guard that let one through would render the app without a
   * token.
   */
  function handleOutcome(outcome: AuthOutcome): SignInOutcome {
    if (outcome.kind === 'authenticated') {
      adopt(outcome.session)
      return 'authenticated'
    }

    mfaTicket.value = outcome.ticket
    mfaPurpose.value = outcome.kind === 'mfa_enrollment_required' ? 'enroll' : 'verify'
    mfaTicketExpiresAt.value = Date.now() + MFA_TICKET_TTL_MS
    status.value = 'anonymous'
    return outcome.kind
  }

  async function signIn(emailAddress: string, password: string): Promise<SignInOutcome> {
    email.value = emailAddress

    let outcome
    try {
      outcome = await loginRequest(emailAddress, password)
    } catch (error) {
      // A refused sign-in still answers the question `status` exists to ask:
      // there is no session. Leaving it at 'unknown' keeps a route guard waiting
      // for a restore that is never coming, which presents as a permanent splash
      // screen after one mistyped password.
      status.value = 'anonymous'
      throw error
    }

    return handleOutcome(outcome)
  }

  /**
   * The password step of an onboarding link. It answers exactly what `signIn`
   * does, so the page routes with the same map: an admin invite stops at
   * enrollment, a TOTP user's reset at the factor prompt, everyone else lands in
   * the app.
   */
  async function redeemOnboard(token: string, password: string): Promise<SignInOutcome> {
    let outcome
    try {
      outcome = await onboardRedeemRequest(token, password)
    } catch (error) {
      status.value = 'anonymous'
      throw error
    }

    return handleOutcome(outcome)
  }

  /** Fetches the pending secret for the enrollment challenge in hand. */
  async function beginEnrollment(): Promise<EnrollmentSecret> {
    const ticket = mfaTicket.value
    if (ticket === null || mfaPurpose.value !== 'enroll') {
      throw new Error('no enrollment challenge is in progress')
    }
    return mfaEnroll(ticket)
  }

  /**
   * Confirms the first TOTP and returns the recovery codes. With a ticket the
   * server also issues the session, which `adopt` takes here, so enrollment is
   * the sign-in.
   */
  async function confirmEnrollment(code: string): Promise<string[]> {
    const ticket = mfaTicket.value
    if (ticket === null || mfaPurpose.value !== 'enroll') {
      throw new Error('no enrollment challenge is in progress')
    }
    const confirmation = await mfaConfirm(ticket, code)
    if (confirmation.session !== null) adopt(confirmation.session)
    return confirmation.recoveryCodes
  }

  async function submitMfaCode(code: string): Promise<void> {
    const ticket = mfaTicket.value
    if (ticket === null || mfaPurpose.value !== 'verify') {
      throw new Error('no second-factor challenge is in progress')
    }
    adopt(await mfaVerify(ticket, code))
  }

  /**
   * Replaces the access token using the refresh cookie. Called by the API client
   * when a request is refused with a code that means the token is unusable, and
   * by `restore` on a page load.
   *
   * A failure clears the session rather than leaving a half-dead one behind. The
   * alternative is a UI that still looks signed in and refuses every call, which
   * is worse than a login screen: it hides the cause.
   */
  async function refresh(): Promise<void> {
    try {
      adopt(await refreshSession())
    } catch (error) {
      clear()
      throw error
    }
  }

  /**
   * One attempt to restore a session from the cookie, for a page reload. A false
   * is not an error: it is a visitor who has not signed in yet, and the caller's
   * job is to show the login screen rather than a failure.
   */
  async function restore(): Promise<boolean> {
    try {
      await refresh()
      return true
    } catch {
      return false
    }
  }

  async function signOut(): Promise<void> {
    try {
      await logoutRequest()
    } catch {
      // Swallowed rather than rethrown, because this does not change what the
      // caller should do: the session ends here either way, and a rejection would
      // stop a sign-out handler before it navigates, leaving the user in an app
      // they are no longer signed in to. Worth being honest about the cost: the
      // cookie is the server's to clear, and until it does, it is still valid.
    }
    clear()
  }

  // Registered here rather than in the app bootstrap so that any store instance
  // wires the client, tests included, and so the client never imports the store.
  setRefreshHandler(async () => {
    await refresh()
  })

  return {
    accessToken,
    user,
    expiresAt,
    status,
    mfaTicket,
    mfaPurpose,
    mfaTicketExpiresAt,
    email,
    roles,
    displayName,
    signIn,
    redeemOnboard,
    beginEnrollment,
    confirmEnrollment,
    submitMfaCode,
    refresh,
    restore,
    signOut,
    clear,
  }
})
