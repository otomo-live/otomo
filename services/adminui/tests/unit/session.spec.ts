import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { request, resetClient, setAccessToken } from '@/api/client'
import { ApiError } from '@/api/errors'
import { useSessionStore } from '@/stores/session'
import { server } from '@/mocks/server'
import { http, HttpResponse } from 'msw'

const CONFIG_NAMESPACES = '/api/admin/config/namespaces'

/**
 * The session store, against the fixture API. What matters here is not that a
 * token arrives but that the client is holding it, that the store's status says
 * what it says, and that signing out leaves nothing behind.
 */

/** Captures the Authorization header of the next call, whatever it is. */
function captureAuthorization(): { current: () => string | null } {
  let seen: string | null = null
  server.use(
    http.get(CONFIG_NAMESPACES, ({ request: incoming }) => {
      seen = incoming.headers.get('Authorization')
      return HttpResponse.json({ namespaces: [] })
    }),
  )
  return { current: () => seen }
}

beforeEach(async () => {
  setActivePinia(createPinia())
  resetClient()
  // The fixture API remembers the last login in module state, so without this a
  // test that signs in would restore a session in the next one.
  await fetch('/admin-auth/logout', { method: 'POST' })
})

describe('signing in', () => {
  it('adopts the session and hands the token to the client', async () => {
    const store = useSessionStore()
    expect(store.status).toBe('unknown')

    const outcome = await store.signIn('admin@example.com', 'correct horse battery staple')

    expect(outcome).toBe('authenticated')
    expect(store.status).toBe('authenticated')
    expect(store.user?.name).toBe('Admin')
    // Exactly ["admin"]: the fixture derives roles from the email's local part,
    // so this is the case that catches a store that invents a role ladder.
    expect(store.roles).toEqual(['admin'])
    expect(store.expiresAt).toBeGreaterThan(Date.now())

    const seen = captureAuthorization()
    await request(CONFIG_NAMESPACES)
    expect(seen.current()).toBe(`Bearer ${store.accessToken ?? ''}`)
  })

  it('asks for the second factor instead of establishing a session', async () => {
    const store = useSessionStore()

    const outcome = await store.signIn('mfa.ops@example.com', 'x')

    expect(outcome).toBe('mfa_required')
    expect(store.mfaTicket).toBeTypeOf('string')
    // Not 'authenticated': no token has been issued, so a guard that trusted the
    // status here would render the app for someone with no credentials.
    expect(store.status).toBe('anonymous')
    expect(store.accessToken).toBeNull()

    await store.submitMfaCode('123456')

    expect(store.status).toBe('authenticated')
    // From the local part `mfa.ops`, title-cased by the fixture.
    expect(store.user?.name).toBe('Mfa Ops')
    expect(store.mfaTicket).toBeNull()
  })

  it('surfaces a refusal as an ApiError and stays anonymous', async () => {
    const store = useSessionStore()
    server.use(
      http.post('/admin-auth/login', () =>
        HttpResponse.json(
          { error: { code: 'invalid_credentials', message: 'no', request_id: 'req_x' } },
          { status: 401 },
        ),
      ),
    )

    const error = (await store
      .signIn('admin@example.com', 'wrong')
      .catch((caught: unknown) => caught)) as ApiError

    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('invalid_credentials')
    expect(store.status).toBe('anonymous')
    expect(store.accessToken).toBeNull()
  })
})

describe('restoring a session on load', () => {
  it('reports false and stays anonymous when there is no cookie to redeem', async () => {
    const store = useSessionStore()

    await expect(store.restore()).resolves.toBe(false)
    expect(store.status).toBe('anonymous')
  })

  it('restores after a previous login in the same browser', async () => {
    const first = useSessionStore()
    await first.signIn('admin@example.com', 'x')

    // A reload: a new store with nothing in memory, which is the whole point of
    // the refresh cookie.
    setActivePinia(createPinia())
    const reloaded = useSessionStore()
    expect(reloaded.status).toBe('unknown')

    await expect(reloaded.restore()).resolves.toBe(true)
    expect(reloaded.status).toBe('authenticated')
    expect(reloaded.roles).toEqual(['admin'])
  })
})

describe('signing out', () => {
  it('clears the session and stops sending the token', async () => {
    const store = useSessionStore()
    await store.signIn('admin@example.com', 'x')

    await store.signOut()

    expect(store.status).toBe('anonymous')
    expect(store.user).toBeNull()
    expect(store.accessToken).toBeNull()

    // Not just null in the store: the client must not put it on a request either.
    const seen = captureAuthorization()
    setAccessToken(null)
    await request(CONFIG_NAMESPACES)
    expect(seen.current()).toBeNull()
  })

  it('ends the session even when the server is unreachable', async () => {
    const store = useSessionStore()
    await store.signIn('admin@example.com', 'x')
    server.use(http.post('/admin-auth/logout', () => HttpResponse.error()))

    // Resolves rather than rejecting: a sign-out that threw would stop the
    // caller's navigation and strand the user in an app they have left.
    await expect(store.signOut()).resolves.toBeUndefined()
    expect(store.status).toBe('anonymous')
  })
})
