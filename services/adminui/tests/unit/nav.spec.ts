import { describe, expect, it } from 'vitest'

import { routes } from '@/router'
import { breadcrumbs, NAV_GROUP_ORDER, navGroups, paletteEntries } from '@/router/nav'

/**
 * The navigation model derived from the route table.
 *
 * These are the properties the shell and the palette both rely on: the role
 * filter matches the guard's own `hasRoleAtLeast`, the order is explicit,
 * empty groups disappear, and every route marked `nav` names a real group so a
 * future ticket cannot add a page that never shows up.
 */

describe('navGroups', () => {
  it('shows a viewer the Operate section and the namespace list it may read', () => {
    const sections = navGroups(routes, 'viewer')

    expect(sections.map((section) => section.group)).toEqual(['operate', 'content'])
    expect(sections[0].label).toBe('Operate')
    expect(sections[0].items.map((item) => item.label)).toEqual(['Overview', 'Logs', 'Audit'])
    expect(sections[0].items.map((item) => item.path)).toEqual([
      '/dashboard',
      '/dashboard/logs',
      '/dashboard/audit',
    ])
    // The list route is `viewer` because GET /namespaces is a viewer read; the
    // editor below it stays live_ops and is not in the navigation at all.
    expect(sections[1].items.map((item) => item.label)).toEqual(['Namespaces', 'Packs', 'Releases'])
  })

  it('shows Content to live_ops and admin too', () => {
    const liveOps = navGroups(routes, ['live_ops'])
    expect(liveOps.map((section) => section.group)).toEqual(['operate', 'content'])
    expect(liveOps[1].label).toBe('Content')
    expect(liveOps[1].items.map((item) => item.label)).toEqual(['Namespaces', 'Packs', 'Releases'])

    const admin = navGroups(routes, ['admin'])
    expect(admin.map((section) => section.group)).toEqual(['operate', 'content', 'admin'])
    expect(admin[2].label).toBe('Admin')
    expect(admin[2].items.map((item) => item.label)).toEqual(['Users'])
    expect(admin[2].items[0].path).toBe('/users')
  })

  it('drops a group with no visible item', () => {
    const sections = navGroups(routes, 'viewer')
    // A viewer reaches neither /users nor any other admin route, so the section
    // must not render empty.
    expect(sections.map((section) => section.group)).not.toContain('admin')
    expect(sections.every((section) => section.items.length > 0)).toBe(true)
  })

  it('hides Users from live_ops and shows it to admin', () => {
    const labels = (roles: string) =>
      navGroups(routes, roles).flatMap((section) => section.items.map((item) => item.label))
    expect(labels('live_ops')).not.toContain('Users')
    expect(labels('viewer')).not.toContain('Users')
    expect(labels('admin')).toContain('Users')
  })

  it('treats a single role string like a one-role list', () => {
    expect(navGroups(routes, 'viewer')).toEqual(navGroups(routes, ['viewer']))
  })

  it('gives every nav:true route a known navGroup', () => {
    for (const route of routes) {
      if (route.meta?.nav !== true) continue
      expect(NAV_GROUP_ORDER).toContain(route.meta.navGroup)
      expect(typeof route.meta.title).toBe('string')
    }
  })
})

describe('paletteEntries', () => {
  it('adds the account page for every signed-in role without putting it in a nav group', () => {
    // It is linked from the sidebar footer, not the sections.
    for (const roles of [['viewer'], ['live_ops'], ['admin']]) {
      const paths = navGroups(routes, roles).flatMap((section) =>
        section.items.map((item) => item.path),
      )
      expect(paths).not.toContain('/account')
      const account = paletteEntries(routes, roles).find((entry) => entry.path === '/account')
      expect(account?.label).toBe('Account')
    }
  })

  it('still contains the role-filtered navigation it is built from', () => {
    const labels = paletteEntries(routes, ['admin']).map((entry) => entry.label)
    expect(labels).toContain('Overview')
    expect(labels).toContain('Users')
    expect(labels).toContain('Account')
  })
})

describe('breadcrumbs', () => {
  it('returns the list route as the only, current crumb', () => {
    expect(breadcrumbs({ name: 'config-namespaces' }, routes)).toEqual([
      { label: 'Namespaces', to: null },
    ])
  })

  it('links the list from the editor and names the namespace as the last crumb', () => {
    expect(
      breadcrumbs(
        { name: 'config-namespace-editor', params: { namespace: 'balance.weapons' } },
        routes,
      ),
    ).toEqual([
      { label: 'Namespaces', to: '/config' },
      { label: 'balance.weapons', to: null },
    ])
  })

  it('resolves a URL-encoded namespace from the route param', () => {
    expect(
      breadcrumbs(
        { name: 'config-namespace-editor', params: { namespace: 'ui.presentation' } },
        routes,
      )[1].label,
    ).toBe('ui.presentation')
  })

  it('hangs the schema page off the namespace list', () => {
    expect(
      breadcrumbs({ name: 'config-namespace-schema', params: { name: 'balance.weapons' } }, routes),
    ).toEqual([
      { label: 'Namespaces', to: '/config' },
      { label: 'balance.weapons', to: null },
    ])
  })

  it('has no crumbs for a route without a name', () => {
    expect(breadcrumbs({ params: {} }, routes)).toEqual([])
  })
})
