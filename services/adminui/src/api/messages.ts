import { ApiError } from '@/api/errors'

/**
 * Turning a failed call into something worth showing a person.
 *
 * The COM-5 envelope carries a machine code and a message written for a
 * developer, and `invalid_credentials` is a poor thing to put in front of
 * someone who mistyped a password. The mapping is here rather than in each view
 * so the sign-in screen, the second-factor screen and every editor cannot drift
 * apart on what a code means.
 *
 * It began in `auth/` and has since moved to `api/`, which is where it belongs:
 * the table is about COM-5 codes, not about signing in, and a Config view that
 * has to import from `auth/` to describe a 409 would be a name lying about the
 * dependency.
 *
 * The request id is kept and shown. It is the one thing in a failure that a
 * support conversation can act on, and it costs a line of small print.
 */

export interface FailureMessage {
  summary: string
  detail: string
  /** The COM-5 request id, when the server sent one. */
  reference: string | null
}

const SUMMARIES: Record<string, string> = {
  invalid_credentials: 'That email and password do not match an account.',
  missing_token: 'Your session has ended. Sign in again.',
  expired: 'Your session has ended. Sign in again.',
  invalid_token: 'Your session has ended. Sign in again.',
  invalid_signature: 'Your session has ended. Sign in again.',
  aud_mismatch: 'Your session has ended. Sign in again.',
  iss_mismatch: 'Your session has ended. Sign in again.',
  insufficient_role: 'That account does not have access to this area.',
  validation_failed: 'The form was not accepted. Check the fields and try again.',
  network_error: 'The server could not be reached.',
  not_found: 'That is no longer there. It may have been removed.',
  // Config's 409. It is described here like any other refusal, but a caller that
  // can do something better than show a message (the draft editor can offer the
  // reload-or-overwrite choice) branches on `isStaleRevision` first and never
  // reaches this entry.
  stale_revision: 'Someone else saved this draft first.',
  stale_schema: 'Someone else saved a newer schema first.',
  precondition_required: 'This change must name the version it was based on. Reload and try again.',
  // User management. The root and self refusals are the server
  // restating a D3 rule; a person meeting one has usually reached a control this
  // page should not have offered in the first place.
  root_protected: 'The root account can only be changed through the CLI.',
  self_modification: 'You cannot change your own account.',
  mfa_not_enrolled: 'That account has no second factor to reset.',
  invite_pending: 'A pending invite already exists for that address.',
  already_exists: 'That already exists.',
  // Onboarding and MFA. A dead link and a spent/expired ticket are
  // each the server restating something the caller can only answer by starting
  // over, so the sentence says where to go rather than what went wrong.
  invalid_link: 'This link is invalid or has expired; ask an admin for a new one.',
  invalid_ticket: 'That setup has expired. Sign in again to restart it.',
  invalid_code: 'That code is not valid. Check the app and try again.',
  mfa_already_enabled: 'This account already has a second factor.',
  mfa_not_enrolling: 'Start setting up the second factor first.',
  // The account page. Turning a factor off is refused for an account
  // that is required to have one, and the root account cannot have one at all.
  mfa_required: 'Two-factor authentication is required for admin accounts.',
  mfa_not_allowed: 'The root account is password-only.',
}

const DEFAULT_SUMMARY = 'That did not work.'

export function describeFailure(error: unknown): FailureMessage {
  if (error instanceof ApiError) {
    return {
      summary: SUMMARIES[error.code] ?? DEFAULT_SUMMARY,
      // The server's own words, kept even when they duplicate the summary: for
      // an unmapped code they are all there is.
      detail: error.message,
      reference: error.requestId,
    }
  }

  // Not an ApiError, so nothing in this stack produced it: a bug of ours, and
  // the message is a developer's rather than a user's.
  return {
    summary: DEFAULT_SUMMARY,
    detail: error instanceof Error ? error.message : String(error),
    reference: null,
  }
}
