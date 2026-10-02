import { HttpResponse, http } from 'msw'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type RouteRecordRaw, type Router } from 'vue-router'
import { beforeEach, describe, expect, it } from 'vitest'

import { DENIED_PATH, installGuards, LOGIN_PATH, MFA_PATH } from '@/auth/guard'
import { sanitiseInternalPath } from '@/auth/paths'
import { resetClient } from '@/api/client'
import { routes } from '@/router'
import { server } from '@/mocks/server'
import { useSessionStore } from '@/stores/session'

/**
 * The navigation policy, against the real route table.
 *
 * The route records are reused as they are written, with their components
 * replaced by a stub: `meta` is what the guard reads, and loading the real views
 * would drag Ionic into a test that never mounts a component. A change to a
 * route's `minRole` or `public` therefore does change what these cases assert,
 * which is the point of building the router from `routes` rather than from a
 * copy of the policy.
 */

function testRouter(): Router {
  // The cast is needed because `RouteRecordRaw` is a union discriminated by
  // `component` versus `components` versus `redirect`, and spreading a member of
  // it widens to a shape that is no longer any single member.
  const testRoutes = routes.map((route) => ({
    ...route,
    component: { template: '<div />' },
  })) as RouteRecordRaw[]
  const router = createRouter({ history: createMemoryHistory(), routes: testRoutes })
  installGuards(router)
  return router
}

/** True when navigation completed on `path`, false when it was redirected. */
async function go(router: Router, path: string): Promise<string> {
  await router.push(path).catch(() => undefined)
  return router.currentRoute.value.fullPath
}

beforeEach(async () => {
  setActivePinia(createPinia())
  resetClient()
  // The fixture API remembers the last login in module state, so a test that
  // signs in would otherwise leave a session behind for the next one.
  await fetch('/admin-auth/logout', { method: 'POST' })
})

describe('a visitor with no session', () => {
  it('is sent to the sign-in screen carrying where they were going', async () => {
    const router = testRouter()

    expect(await go(router, '/config')).toBe(`${LOGIN_PATH}?returnTo=/config`)
  })

  it('keeps the query string of a deep link', async () => {
    const router = testRouter()

    expect(await go(router, '/config/namespaces/balance.weapons')).toBe(
      `${LOGIN_PATH}?returnTo=/config/namespaces/balance.weapons`,
    )
  })

  it('reaches the sign-in screen and the second-factor screen', async () => {
    const router = testRouter()

    expect(await go(router, LOGIN_PATH)).toBe(LOGIN_PATH)
    // Public, and it decides for itself whether a challenge is in progress.
    expect(await go(router, MFA_PATH)).toBe(MFA_PATH)
  })
})

describe('a session restored from the refresh cookie', () => {
  it('lets a reload land on the deep link instead of bouncing to sign-in', async () => {
    const first = useSessionStore()
    await first.signIn('admin@example.com', 'x')

    // A reload: a new store with nothing in memory and a new router.
    setActivePinia(createPinia())
    const reloaded = useSessionStore()
    expect(reloaded.status).toBe('unknown')

    const router = testRouter()
    expect(await go(router, '/config')).toBe('/config')
    // The guard asked once, and the answer was a session.
    expect(reloaded.status).toBe('authenticated')
  })
})

describe('role gating', () => {
  it('admits a signed-in viewer to the editor, which is now a viewer read', async () => {
    const store = useSessionStore()
    await store.signIn('viewer@example.com', 'x')

    const router = testRouter()
    // The editor route dropped to viewer: its form, JSON and
    // version history are all viewer reads, and the page renders its mutating
    // controls read-only for anyone below live_ops. The guard's job is only to
    // let the role reach the page.
    expect(await go(router, '/config/namespaces/balance.weapons')).toBe(
      '/config/namespaces/balance.weapons',
    )
  })

  it('admits the same viewer to the dashboard and the read-only namespace list', async () => {
    const store = useSessionStore()
    await store.signIn('viewer@example.com', 'x')

    const router = testRouter()
    expect(await go(router, '/dashboard')).toBe('/dashboard')
    // GET /namespaces is a viewer read, so the list route dropped with it.
    expect(await go(router, '/config')).toBe('/config')
  })

  it('admits liveops to both, since live_ops is above viewer', async () => {
    const store = useSessionStore()
    await store.signIn('liveops@example.com', 'x')

    const router = testRouter()
    expect(await go(router, '/dashboard')).toBe('/dashboard')
    expect(await go(router, '/config')).toBe('/config')
    // The editor's save/validate actions are live_ops operations, so the editor
    // itself stays reachable at live_ops.
    expect(await go(router, '/config/namespaces/balance.weapons')).toBe(
      '/config/namespaces/balance.weapons',
    )
  })

  it('denies the dashboard too when no role can be ranked', async () => {
    // The case a redirect-to-home policy would loop on: denied at /dashboard,
    // sent to /dashboard. The denied screen is public, so this terminates.
    //
    // The fixture derives roles from the email and has no way to issue an
    // unknown one, so the login response is replaced here. That is the point of
    // the case: a server that starts sending a role this build predates must
    // lock the SPA down rather than let it assume the role is harmless.
    server.use(
      http.post('/admin-auth/login', () =>
        HttpResponse.json({
          access_token: 'tok_auditor',
          expires_in: 3600,
          user: { id: 'usr_auditor', name: 'Auditor', roles: ['auditor'] },
        }),
      ),
    )
    const store = useSessionStore()
    await store.signIn('auditor@example.com', 'x')
    expect(store.roles).toEqual(['auditor'])

    const router = testRouter()
    expect(await go(router, '/dashboard')).toBe(`${DENIED_PATH}?area=/dashboard`)
  })
})

describe('a signed-in visitor on the sign-in screen', () => {
  it('is sent onwards, honouring a returnTo', async () => {
    const store = useSessionStore()
    await store.signIn('admin@example.com', 'x')

    const router = testRouter()
    expect(await go(router, '/login?returnTo=/config/namespaces/x')).toBe('/config/namespaces/x')
  })

  it('is sent home when the returnTo is not a path in this app', async () => {
    const store = useSessionStore()
    await store.signIn('admin@example.com', 'x')

    const router = testRouter()
    expect(await go(router, '/login?returnTo=https://evil.example/')).toBe('/dashboard')
    expect(await go(router, '/login?returnTo=//evil.example/')).toBe('/dashboard')
  })
})

describe('sanitiseInternalPath', () => {
  it('accepts a path and its query, and refuses everything else', () => {
    expect(sanitiseInternalPath('/config')).toBe('/config')
    expect(sanitiseInternalPath('/config?tab=raw#top')).toBe('/config?tab=raw#top')

    expect(sanitiseInternalPath('https://evil.example')).toBeNull()
    expect(sanitiseInternalPath('//evil.example')).toBeNull()
    // Normalised to '//' by some browsers, so refused for the same reason.
    expect(sanitiseInternalPath('/\\evil.example')).toBeNull()
    expect(sanitiseInternalPath('config')).toBeNull()
    expect(sanitiseInternalPath('')).toBeNull()
    expect(sanitiseInternalPath(undefined)).toBeNull()
    // An array, which is what a repeated query parameter arrives as.
    expect(sanitiseInternalPath(['/config', '/dashboard'])).toBeNull()
    expect(sanitiseInternalPath(`/${'a'.repeat(600)}`)).toBeNull()
  })
})
