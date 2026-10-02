import { HttpResponse, http } from 'msw'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import { refreshSession } from '@/api/auth'
import {
  configureClient,
  request,
  requestUnauthenticated,
  resetClient,
  setAccessToken,
  setRefreshHandler,
} from '@/api/client'
import { ApiError } from '@/api/errors'
import { server } from '@/mocks/server'

/**
 * The transport's four behaviours, each against the live fixture API rather than
 * a stub, so what is tested is the request the SPA really makes. `server` comes
 * from tests/unit/setup/msw.ts, and `server.use` registers a handler in front of
 * the shared ones for the duration of one test.
 */

const CONFIG_NAMESPACES = '/api/admin/config/namespaces'
const REFRESH = '/admin-auth/refresh'

/** The shape of a refusal, at the one place these tests need to build one. */
function refusal(status: number, code: string) {
  return HttpResponse.json(
    { error: { code, message: `refused with ${code}`, request_id: 'req_test' } },
    { status },
  )
}

function issuedSession(token: string) {
  return HttpResponse.json({
    access_token: token,
    expires_in: 3600,
    user: { id: 'usr_admin', name: 'Admin', roles: ['admin'] },
  })
}

/** Every case starts from a client with no token, no handler and no generation. */
beforeEach(() => {
  resetClient()
})

afterEach(() => {
  resetClient()
})

describe('the COM-5 envelope', () => {
  it('maps a refusal onto the code, the message and the request id', async () => {
    const error = (await request('/api/admin/config/namespaces/nope/draft').catch(
      (caught: unknown) => caught,
    )) as ApiError

    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(404)
    expect(error.code).toBe('not_found')
    expect(error.requestId).toBeTypeOf('string')
  })

  it('resolves with the parsed body on success', async () => {
    const body = await request<{ namespaces: { name: string }[] }>(CONFIG_NAMESPACES)
    expect(body.namespaces.map((namespace) => namespace.name)).toContain('balance.weapons')
  })

  it('prefixes a request with the configured base URL, without doubling the slash', async () => {
    let seen = ''
    server.use(
      http.get('/gateway/api/admin/config/namespaces', ({ request: incoming }) => {
        seen = new URL(incoming.url).pathname
        return HttpResponse.json({ namespaces: [] })
      }),
    )

    configureClient({ baseUrl: '/gateway/' })
    try {
      await request(CONFIG_NAMESPACES)
    } finally {
      // resetClient deliberately leaves the configured base URL alone, so a test
      // that changes it has to change it back for the next one.
      configureClient({ baseUrl: '' })
    }

    expect(seen).toBe('/gateway/api/admin/config/namespaces')
  })

  it('still produces an ApiError when the body is not an envelope', async () => {
    // What a gateway answers when an upstream is not deployed: a 502 whose body
    // is whatever the proxy felt like sending.
    server.use(
      http.get(
        CONFIG_NAMESPACES,
        () =>
          new HttpResponse('<html>bad gateway</html>', {
            status: 502,
            headers: { 'Content-Type': 'text/html' },
          }),
      ),
    )

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(502)
    expect(error.code).toBe('internal')
    expect(error.requestId).toBeNull()
  })

  it('names a transport failure rather than inventing an HTTP status', async () => {
    server.use(http.get(CONFIG_NAMESPACES, () => HttpResponse.error()))

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(error.code).toBe('network_error')
    expect(error.status).toBe(0)
  })
})

describe('the bearer token', () => {
  it('is sent when a token is set', async () => {
    let seen: string | null = null
    server.use(
      http.get(CONFIG_NAMESPACES, ({ request: incoming }) => {
        seen = incoming.headers.get('Authorization')
        return HttpResponse.json({ namespaces: [] })
      }),
    )

    setAccessToken('tok_abc')
    await request(CONFIG_NAMESPACES)
    expect(seen).toBe('Bearer tok_abc')
  })

  it('is not sent by the unauthenticated path, so a stale token cannot reach a login', async () => {
    let seen: string | null = 'not set'
    server.use(
      http.post('/admin-auth/logout', ({ request: incoming }) => {
        seen = incoming.headers.get('Authorization')
        return new HttpResponse(null, { status: 204 })
      }),
    )

    setAccessToken('tok_stale')
    await requestUnauthenticated('/admin-auth/logout', { method: 'POST' })
    expect(seen).toBeNull()
  })
})

describe('refreshing after an expiry', () => {
  let refreshCalls = 0
  let tokenIsFresh = false

  /**
   * Refuses every config call until a refresh has happened, and counts the
   * refreshes. This is the smallest world in which the single-flight behaviour is
   * observable at all.
   */
  function expireEveryToken(): void {
    refreshCalls = 0
    tokenIsFresh = false
    server.use(
      http.get(CONFIG_NAMESPACES, () =>
        tokenIsFresh ? HttpResponse.json({ namespaces: [] }) : refusal(401, 'expired'),
      ),
      http.post(REFRESH, () => {
        refreshCalls += 1
        tokenIsFresh = true
        return issuedSession('tok_fresh')
      }),
    )
  }

  beforeEach(() => {
    expireEveryToken()
    // The handler the session store registers, reproduced here so this suite
    // tests the contract the store has to satisfy rather than the store itself.
    setRefreshHandler(async () => {
      const issued = await refreshSession()
      setAccessToken(issued.access_token)
    })
  })

  it('refreshes exactly once for five parallel refusals and lets all five through', async () => {
    setAccessToken('tok_stale')

    const results = await Promise.all(
      Array.from({ length: 5 }, () => request<{ namespaces: unknown[] }>(CONFIG_NAMESPACES)),
    )

    expect(refreshCalls).toBe(1)
    expect(results).toHaveLength(5)
    for (const result of results) expect(result.namespaces).toEqual([])
  })

  it('retries at most once, so a token refused again does not loop', async () => {
    // The refresh succeeds but the new token is refused too: a clock skew, a
    // gateway that has not picked up the new key. The retry must not try again.
    server.use(
      http.post(REFRESH, () => {
        refreshCalls += 1
        return issuedSession('tok_fresh')
      }),
    )

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(error.code).toBe('expired')
    expect(refreshCalls).toBe(1)
  })

  it('does not refresh on a 403, because the token is not the problem', async () => {
    server.use(http.get(CONFIG_NAMESPACES, () => refusal(403, 'insufficient_role')))

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(error.status).toBe(403)
    expect(error.code).toBe('insufficient_role')
    expect(refreshCalls).toBe(0)
  })

  it('leaves the refusal alone when nothing can refresh it', async () => {
    setRefreshHandler(null)

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(error.code).toBe('expired')
    expect(refreshCalls).toBe(0)
  })

  it('reports the original refusal when the refresh itself fails', async () => {
    let attempts = 0
    server.use(
      http.post(REFRESH, () => {
        attempts += 1
        return refusal(401, 'invalid_credentials')
      }),
    )

    const error = (await request(CONFIG_NAMESPACES).catch((caught: unknown) => caught)) as ApiError
    expect(attempts).toBe(1)
    // The store clears the session when this happens; the caller still sees why
    // the request it made was refused, not the failure of the recovery attempt.
    expect(error.code).toBe('expired')
  })
})
