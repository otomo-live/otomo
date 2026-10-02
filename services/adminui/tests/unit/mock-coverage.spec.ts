import { describe, expect, it } from 'vitest'

import { handlers } from '@/mocks/handlers'

/**
 * Every route the admin UI can call must have a fixture handler.
 *
 * The list is data rather than prose on purpose: when the gateway gains a route
 * and the SPA starts calling it, the missing mock shows up as a failing test
 * rather than as an unhandled-request connection error at runtime. The paths are
 * through `gateway_dev`, unstripped, exactly as the SPA addresses them.
 *
 * Handlers are matched by `info.method`/`info.path`, so a renamed path fails.
 * Path parameters are compared structurally (`:channel` matches both `{ch}` and a
 * literal such as `live`), which is what lets the one
 * `POST .../channels/:channel/releases` handler cover the separate `live` route.
 */

interface Route {
  method: string
  path: string
}

const ROUTES: Route[] = [
  { method: 'DELETE', path: '/api/admin/users/invites/{id}' },
  { method: 'GET', path: '/admin-auth/account/sessions' },
  { method: 'GET', path: '/admin-auth/audit' },
  { method: 'GET', path: '/admin-auth/me' },
  { method: 'GET', path: '/api/admin/config/audit' },
  { method: 'GET', path: '/api/admin/config/channels/{ch}/releases' },
  { method: 'GET', path: '/api/admin/config/namespaces' },
  { method: 'GET', path: '/api/admin/config/namespaces/{ns}/diff' },
  { method: 'GET', path: '/api/admin/config/namespaces/{ns}/draft' },
  { method: 'GET', path: '/api/admin/config/namespaces/{ns}/schema' },
  { method: 'GET', path: '/api/admin/config/namespaces/{ns}/versions' },
  { method: 'GET', path: '/api/admin/config/namespaces/{ns}/versions/{v}' },
  { method: 'GET', path: '/api/admin/config/packs' },
  { method: 'GET', path: '/api/admin/dashboard/audit' },
  { method: 'GET', path: '/api/admin/dashboard/logs' },
  { method: 'GET', path: '/api/admin/dashboard/logs/tail' },
  { method: 'GET', path: '/api/admin/dashboard/overview' },
  { method: 'GET', path: '/api/admin/dashboard/services' },
  { method: 'GET', path: '/api/admin/dashboard/services/{name}/series' },
  { method: 'GET', path: '/api/admin/users' },
  { method: 'GET', path: '/api/admin/users/invites' },
  { method: 'PATCH', path: '/api/admin/users/{id}' },
  { method: 'POST', path: '/admin-auth/account/mfa/disable' },
  { method: 'POST', path: '/admin-auth/account/mfa/recovery-codes' },
  { method: 'POST', path: '/admin-auth/account/password' },
  { method: 'POST', path: '/admin-auth/account/sessions/revoke-others' },
  { method: 'POST', path: '/admin-auth/login' },
  { method: 'POST', path: '/admin-auth/logout' },
  { method: 'POST', path: '/admin-auth/mfa/confirm' },
  { method: 'POST', path: '/admin-auth/mfa/enroll' },
  { method: 'POST', path: '/admin-auth/mfa/verify' },
  { method: 'POST', path: '/admin-auth/onboard' },
  { method: 'POST', path: '/admin-auth/onboard/lookup' },
  { method: 'POST', path: '/admin-auth/refresh' },
  { method: 'POST', path: '/api/admin/config/channels/live/releases' },
  { method: 'POST', path: '/api/admin/config/channels/{ch}/promote' },
  { method: 'POST', path: '/api/admin/config/channels/{ch}/releases' },
  { method: 'POST', path: '/api/admin/config/channels/{ch}/rollback' },
  { method: 'POST', path: '/api/admin/config/namespaces' },
  { method: 'POST', path: '/api/admin/config/namespaces/{ns}/draft/validate' },
  { method: 'POST', path: '/api/admin/config/namespaces/{ns}/versions' },
  { method: 'POST', path: '/api/admin/config/packs' },
  { method: 'POST', path: '/api/admin/users' },
  { method: 'POST', path: '/api/admin/users/{id}/mfa/reset' },
  { method: 'POST', path: '/api/admin/users/{id}/reset' },
  { method: 'PUT', path: '/api/admin/config/namespaces/{ns}/draft' },
  { method: 'PUT', path: '/api/admin/config/namespaces/{ns}/schema' },
]

function segments(path: string): string[] {
  return path.split('/').filter((segment) => segment.length > 0)
}

/** `{name}` in the required list is a wildcard for one path segment. */
function isPlaceholder(segment: string): boolean {
  return segment.startsWith('{') && segment.endsWith('}')
}

/** Does one handler path serve the required path? Params absorb one segment. */
function pathsMatch(pattern: string, required: string): boolean {
  const left = segments(pattern)
  const right = segments(required)
  if (left.length !== right.length) return false
  return left.every((segment, index) => {
    const wanted = right[index]
    if (isPlaceholder(wanted)) return true
    if (segment.startsWith(':')) return true
    return segment === wanted
  })
}

function hasHandler(route: Route): boolean {
  return handlers.some(
    (handler) =>
      String(handler.info.method).toUpperCase() === route.method &&
      pathsMatch(String(handler.info.path), route.path),
  )
}

describe('the fixture API covers every route the SPA calls', () => {
  it.each(ROUTES)('$method $path', (route) => {
    expect(hasHandler(route), `no MSW handler serves ${route.method} ${route.path}`).toBe(true)
  })

  it('does not pretend an unlisted route is covered', () => {
    expect(hasHandler({ method: 'GET', path: '/api/admin/config/does-not-exist' })).toBe(false)
    expect(hasHandler({ method: 'DELETE', path: '/api/admin/dashboard/overview' })).toBe(false)
  })
})
