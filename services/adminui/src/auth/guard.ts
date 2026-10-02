import type { Router } from 'vue-router'

import { sanitiseInternalPath } from '@/auth/paths'
import { hasRoleAtLeast, type StaffRole } from '@/auth/roles'
import { envConfig } from '@/api/env'
import { useSessionStore } from '@/stores/session'

/**
 * The navigation policy, in one place.
 *
 * Two rules and one restoration:
 *
 *  - a route that is not public needs a session, and a visitor without one is
 *    sent to `/login` carrying the path they wanted;
 *  - a route with a `minRole` needs the session's roles to reach it, and being
 *    signed in is not the same as being allowed.
 *
 * The role check reads `meta.minRole`, which mirrors the gateway's route table
 * (`services/gateway_dev/internal/router/dev.go`) and is a UX mirror only. The
 * gateway refuses the request regardless of what this decided, so a mistake here
 * shows a page that cannot load rather than leaking anything.
 */

export const LOGIN_PATH = '/login'
export const MFA_PATH = '/login/mfa'
export const ENROLL_PATH = '/login/enroll'
export const ONBOARD_PATH = '/onboard'
export const DENIED_PATH = '/denied'

/** The public pages a signed-in visitor has no business on. */
const SIGNED_OUT_ONLY = new Set(['login', 'onboard', 'mfa-enroll'])

/** Where a signed-in visitor lands when nothing better is on offer. */
export const HOME_PATH = '/dashboard'

/**
 * Installed by the router module, and also called directly by the guard tests
 * against a memory-history router built from the same route table.
 */
export function installGuards(router: Router): void {
  router.beforeEach(async (to) => {
    const session = useSessionStore()

    // One question per page load, and the store's status is what makes it once:
    // it starts at 'unknown' and never returns to it. A failed restore lands on
    // 'anonymous', so an unreachable auth service shows the sign-in screen
    // rather than hanging the app on a splash.
    if (session.status === 'unknown') await session.restore()

    if (to.meta.public === true) {
      // A visitor who already has a session has no business on the sign-in,
      // onboarding or enrollment screens, and the back button should not be able
      // to strand them there.
      if (session.status === 'authenticated' && SIGNED_OUT_ONLY.has(String(to.name))) {
        return sanitiseInternalPath(to.query.returnTo) ?? HOME_PATH
      }
      return true
    }

    if (session.status !== 'authenticated') {
      // The whole path, query and all, so a deep link into the editor survives
      // one trip through the sign-in form.
      return { path: LOGIN_PATH, query: { returnTo: to.fullPath } }
    }

    const minRole = to.meta.minRole as StaffRole | undefined
    if (minRole !== undefined && !hasRoleAtLeast(session.roles, minRole)) {
      // Denied rather than bounced to a home page. Redirecting to the dashboard
      // would be a silent no-op for a token whose roles this build cannot rank,
      // and the guard would then refuse the dashboard and redirect again.
      return { path: DENIED_PATH, query: { area: to.fullPath } }
    }

    return true
  })

  // The route's own title, prefixed with the deployment's. Set here rather than
  // per view so a new page cannot forget it. A live deployment also carries a
  // `[LIVE]` prefix, so a tab left open on staging is not mistaken for one on
  // production.
  router.afterEach((to) => {
    const title = typeof to.meta.title === 'string' ? to.meta.title : null
    const app = envConfig().appTitle
    const prefix = envConfig().environment === 'live' ? '[LIVE] ' : ''
    document.title = title === null ? `${prefix}${app}` : `${prefix}${title} - ${app}`
  })
}
