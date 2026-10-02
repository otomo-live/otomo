import { describe, expect, it } from 'vitest'

import { readFragmentToken, stripFragment } from '@/auth/onboard'

/**
 * The fragment is the one URL part a browser does not send with a request, so it
 * is where the onboarding token lives. These cases pin the two halves of handling
 * it: reading the token exactly once, and erasing it without a navigation.
 */

describe('readFragmentToken', () => {
  it('reads a token from a plain fragment', () => {
    expect(readFragmentToken('#token=abc123')).toBe('abc123')
  })

  it('decodes a percent-encoded token', () => {
    expect(readFragmentToken('#token=a%2Fb%2Bc')).toBe('a/b+c')
  })

  it('reads the token from among other fragment fields', () => {
    expect(readFragmentToken('#source=email&token=xyz')).toBe('xyz')
  })

  it('is null for an absent, empty or malformed fragment', () => {
    expect(readFragmentToken('')).toBeNull()
    expect(readFragmentToken('#')).toBeNull()
    expect(readFragmentToken('#token=')).toBeNull()
    expect(readFragmentToken('#other=1')).toBeNull()
  })
})

describe('stripFragment', () => {
  it('replaces the URL with path and query, keeping history state', () => {
    const calls: Array<{ state: unknown; url: string }> = []
    const history = {
      state: { marker: true },
      replaceState(state: unknown, _title: string, url?: string | URL | null) {
        calls.push({ state, url: String(url) })
      },
    }

    stripFragment({ pathname: '/admin/onboard', search: '?returnTo=/dashboard' }, history)

    expect(calls).toEqual([{ state: { marker: true }, url: '/admin/onboard?returnTo=/dashboard' }])
    // The token is in the fragment, which the replacement does not carry.
    expect(calls[0].url).not.toContain('token')
  })
})
