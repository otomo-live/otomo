import { createWebHistory, type RouteRecordRaw, type Router } from 'vue-router'
// From @ionic/vue-router, not vue-router: its createRouter wraps the one it
// re-exports so that `install` provides the `navManager` and `viewStacks` that
// IonRouterOutlet injects. Building the router with vue-router's factory instead
// leaves IonRouterOutlet with nothing to inject and it throws during setup, so
// the app renders its startup-failure panel on every route. Nothing caught that
// until the first browser test.
import { createRouter as createIonicRouter } from '@ionic/vue-router'

import {
  DENIED_PATH,
  ENROLL_PATH,
  HOME_PATH,
  installGuards,
  LOGIN_PATH,
  MFA_PATH,
  ONBOARD_PATH,
} from '@/auth/guard'
import type { StaffRole } from '@/auth/roles'
import type { NavGroup } from '@/router/nav'

/**
 * The route table from WEB-1: `/login`, `/config/*`, `/dashboard/*`.
 *
 * `meta.minRole` is declared here and enforced in the navigation guard
 * (`@/auth/guard`). The navigation model is derived from THIS table rather than
 * kept in a second list, so the menu and the guard cannot disagree about who may
 * see what: `meta.nav` marks what the shell shows, `meta.title` is both the menu
 * label and the document title.
 *
 * The role bar per prefix is set by gateway_dev's route table
 * (internal/router/dev.go), not chosen here:
 *
 *   /api/admin/config/*                              viewer
 *   POST /api/admin/config/channels/live/releases     admin
 *   /api/admin/dashboard/*                            viewer
 *   /api/admin/session/*                              viewer
 *   /api/admin/users*                                 admin
 *
 * `minRole` on a route below is a UX mirror of that. It is NOT the enforcement
 * point: WEB-5 is explicitly UX-only, and the gateway refuses the request
 * regardless of what the SPA renders.
 *
 * The namespace LIST is a viewer read (GET /namespaces is viewer), and since
 * The EDITOR is `viewer` too: form, JSON and version history are all
 * viewer reads, and the editor renders itself read-only below `live_ops`, which
 * is the role its save, validate and create-version actions need. The schema page
 * is `viewer` to read and gates its own Edit button to `admin` (PUT /schema is
 * admin). Stricter than the gateway is safe; looser is not.
 */

declare module 'vue-router' {
  interface RouteMeta {
    /** Reachable without a session. */
    public?: boolean
    /** The lowest role that may open this route. */
    minRole?: StaffRole
    /** The document title, and the menu label when `nav` is set. */
    title?: string
    /** Show in the shell's navigation. */
    nav?: boolean
    /** Show in the ⌘K palette even though it is not in a nav group. */
    palette?: boolean
    /** Which sidebar section the route belongs to. */
    navGroup?: NavGroup
    /** Position within the group; lower comes first. */
    navOrder?: number
    /** The route name this route hangs under in the breadcrumb. */
    crumbParent?: string
  }
}

export const routes: RouteRecordRaw[] = [
  { path: '/', redirect: HOME_PATH },
  {
    path: LOGIN_PATH,
    name: 'login',
    component: () => import('@/views/auth/LoginView.vue'),
    meta: { public: true, title: 'Sign in' },
  },
  {
    path: MFA_PATH,
    name: 'mfa',
    component: () => import('@/views/auth/MfaView.vue'),
    meta: { public: true, title: 'Second factor' },
  },
  {
    // Onboarding. Public, and the link's token rides in the fragment
    // (`…/admin/onboard#token=…`) rather than the path or query. The view reads
    // it from `location.hash` and immediately strips it with `replaceState`.
    path: ONBOARD_PATH,
    name: 'onboard',
    component: () => import('@/views/auth/OnboardView.vue'),
    meta: { public: true, title: 'Set up your account' },
  },
  {
    // TOTP enrollment. Public because no session exists yet: the ticket that
    // authorizes it lives in the session store's memory, never in the URL.
    path: ENROLL_PATH,
    name: 'mfa-enroll',
    component: () => import('@/views/auth/EnrollView.vue'),
    meta: { public: true, title: 'Set up two-factor authentication' },
  },
  {
    path: DENIED_PATH,
    name: 'denied',
    component: () => import('@/views/auth/DeniedView.vue'),
    // Public because a visitor who is refused here has nowhere else to be sent,
    // and because the guard must never be able to bounce off it in a loop.
    meta: { public: true, title: 'Not available' },
  },
  {
    path: '/config',
    name: 'config-namespaces',
    component: () => import('@/views/config/NamespaceListView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Namespaces',
      nav: true,
      navGroup: 'content',
      navOrder: 1,
    },
  },
  {
    path: '/config/packs',
    name: 'config-packs',
    component: () => import('@/views/config/PacksView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Packs',
      nav: true,
      navGroup: 'content',
      navOrder: 2,
    },
  },
  {
    // Reached from the namespace list rather than from the navigation, so it
    // carries no `nav`. `viewer` because the form, the raw JSON and the version
    // history are all viewer reads; the editor's mutating controls (save,
    // validate, create version) are gated to `live_ops` inside the view
    //.
    path: '/config/namespaces/:namespace',
    name: 'config-namespace-editor',
    component: () => import('@/views/config/NamespaceEditorView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Namespace',
      crumbParent: 'config-namespaces',
    },
  },
  {
    // The schema page. `viewer` to read; its Edit button is gated to `admin`,
    // because replacing a schema is a PUT and the gateway's bar is admin. It
    // hangs off the list rather than the editor: the editor is the `:namespace`
    // path, and a parent crumb carrying a parameter this route does not supply
    // would link nowhere.
    path: '/config/namespaces/:name/schema',
    name: 'config-namespace-schema',
    component: () => import('@/views/config/NamespaceSchemaView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Schema',
      crumbParent: 'config-namespaces',
    },
  },
  {
    path: '/dashboard',
    name: 'dashboard-overview',
    component: () => import('@/views/dashboard/OverviewView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Overview',
      nav: true,
      navGroup: 'operate',
      navOrder: 1,
    },
  },
  {
    path: '/dashboard/logs',
    name: 'dashboard-logs',
    component: () => import('@/views/dashboard/LogsView.vue'),
    meta: { minRole: 'viewer', title: 'Logs', nav: true, navGroup: 'operate', navOrder: 2 },
  },
  {
    // The merged audit (DSH-C11). `viewer` like the rest of the dashboard, which
    // is what /api/admin/dashboard/* is gated at in gateway_dev's route table.
    path: '/dashboard/audit',
    name: 'dashboard-audit',
    component: () => import('@/views/dashboard/AuditView.vue'),
    meta: { minRole: 'viewer', title: 'Audit', nav: true, navGroup: 'operate', navOrder: 3 },
  },
  {
    // Reached from an overview card rather than the menu; `crumbParent` walks
    // back to the overview and the dynamic segment supplies the title.
    path: '/dashboard/services/:name',
    name: 'dashboard-service-detail',
    component: () => import('@/views/dashboard/ServiceDetailView.vue'),
    meta: { minRole: 'viewer', title: 'Service', crumbParent: 'dashboard-overview' },
  },
  {
    // The release history, with the channel chosen by `?channel=`. `viewer`
    // because reading a channel's releases is a viewer read; the rollback and
    // promote controls it carries are gated to `admin` inside the view (both
    // POSTs are admin in gateway_dev's table).
    path: '/config/releases',
    name: 'config-releases',
    component: () => import('@/views/config/ReleaseHistoryView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Releases',
      nav: true,
      navGroup: 'content',
      navOrder: 3,
    },
  },
  {
    // The release composer, reached from the namespace list's "Compose release"
    // button rather than the menu, so it carries no `nav`. `live_ops` because
    // publishing to dev or staging is a live_ops act; the page itself refuses
    // `live` to anyone below admin, whose POST the gateway also refuses.
    //
    // Declared BEFORE the release-diff route below: both are two-segment paths
    // under /config/releases, and `:releaseId` would otherwise swallow `compose`.
    path: '/config/releases/:channel/compose',
    name: 'config-release-compose',
    component: () => import('@/views/config/ReleaseComposeView.vue'),
    meta: {
      minRole: 'live_ops',
      title: 'Compose release',
      navGroup: 'content',
      crumbParent: 'config-namespaces',
    },
  },
  {
    // Reached from an audit row's "View changes" link, so it carries no `nav`.
    // It hangs under the namespace list because a release is that list's content
    // frozen and shipped. `viewer` rather than the list's `live_ops`: reading a
    // release is a gateway-`viewer` operation (see the role table above), and the
    // page has no mutations on it.
    path: '/config/releases/:channel/:releaseId',
    name: 'config-release-diff',
    component: () => import('@/views/config/ReleaseDiffView.vue'),
    meta: {
      minRole: 'viewer',
      title: 'Release',
      navGroup: 'content',
      crumbParent: 'config-namespaces',
    },
  },
  {
    // Staff user management. The router path is `/users`, not
    // `/admin/users`: this app's history base is `/admin/` (see `BASE` in
    // vite.config.ts), so `/users` is the browser URL `/admin/users` while the
    // API it calls lives at `/api/admin/users`. Writing `/admin/users` here
    // would nest the same prefix twice.
    //
    // `minRole: admin` mirrors gateway_dev's route table, where every
    // `/api/admin/users*` route requires `admin`; the guard turns a live_ops or
    // viewer URL into `/denied` and `navGroups` omits the item from their menu.
    path: '/users',
    name: 'admin-users',
    component: () => import('@/views/admin/UsersView.vue'),
    meta: {
      minRole: 'admin',
      title: 'Users',
      nav: true,
      navGroup: 'admin',
      navOrder: 1,
    },
  },
  {
    // The signed-in person's own account. No `minRole` and no `nav`:
    // every role reaches it, it is not one of the sidebar groups, and the shell
    // links it from its footer while the palette picks it up via `meta.palette`.
    path: '/account',
    name: 'account',
    component: () => import('@/views/account/AccountView.vue'),
    meta: { title: 'Account', palette: true },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { public: true, title: 'Not found' },
  },
]

export const router: Router = createIonicRouter({
  // '/admin/', from Vite's base. gateway_dev forwards the prefix unstripped,
  // so the app's history base is not '/'.
  history: createWebHistory(import.meta.env.BASE_URL),
  routes,
})

installGuards(router)

export default router
