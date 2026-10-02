import { describe, expect, it } from 'vitest'

import { ApiError } from '@/api/errors'
import { describeFailure } from '@/api/messages'

/**
 * The mapping from a failure to something a person can read. What is worth
 * pinning is that a known code never leaks the machine code into the summary,
 * that an unknown one still says something, and that the request id survives:
 * it is the only part of a failure a support conversation can act on.
 */

function apiError(code: string, message = 'machine words'): ApiError {
  return new ApiError({ status: 401, code, message, requestId: 'req_abc' })
}

describe('describeFailure', () => {
  it('translates a known code rather than showing it', () => {
    const failure = describeFailure(apiError('invalid_credentials'))
    expect(failure.summary).toBe('That email and password do not match an account.')
    expect(failure.detail).toBe('machine words')
    expect(failure.reference).toBe('req_abc')
  })

  it('keeps the server message for a code it does not know', () => {
    const failure = describeFailure(apiError('mfa_ticket_expired', 'ticket is stale'))
    expect(failure.summary).toBe('That did not work.')
    expect(failure.detail).toBe('ticket is stale')
  })

  it('reports a failed fetch as unreachable rather than as a refusal', () => {
    const failure = describeFailure(apiError('network_error'))
    expect(failure.summary).toBe('The server could not be reached.')
  })

  it('says nothing about a token when the session simply ended', () => {
    // A person reading this should not have to know what "expired" refers to.
    for (const code of ['expired', 'invalid_token', 'missing_token']) {
      expect(describeFailure(apiError(code)).summary).toBe('Your session has ended. Sign in again.')
    }
  })

  it('passes through a null request id', () => {
    const error = new ApiError({ status: 502, code: 'internal', message: 'x', requestId: null })
    expect(describeFailure(error).reference).toBeNull()
  })

  it('survives a thrown value that is not an ApiError', () => {
    const failure = describeFailure(new TypeError('fetch failed'))
    expect(failure.summary).toBe('That did not work.')
    expect(failure.detail).toBe('fetch failed')
    expect(failure.reference).toBeNull()
  })
})
