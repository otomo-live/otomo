import type { RouteRecordRaw } from 'vue-router'

import { hasRoleAtLeast } from '@/auth/roles'

/**
 * The navigation model, derived from the route table rather than written a
 * second time in the shell.
 *
 * `navGroups` answers "what does this session see, and in what order" and
 * `breadcrumbs` answers "where am I, given the route's `crumbParent`". Neither
 * touches a component, so both are unit-testable without mounting Ionic.
 *
 * The route table remains the single source: a route appears in the menu when it
 * carries `meta.nav`, lands in the group named by `meta.navGroup`, and is gated
 * by the same `meta.minRole` the navigation guard reads. A page that does not
 * exist yet is simply a route that is not there, and appears the moment its
 * ticket adds one.
 */

/** The three sections of the sidebar, in the order they are rendered. */
export type NavGroup = 'operate' | 'content' | 'admin'

export const NAV_GROUP_ORDER: readonly NavGroup[] = ['operate', 'content', 'admin']

export const NAV_GROUP_LABELS: Record<NavGroup, string> = {
  operate: 'Operate',
  content: 'Content',
  admin: 'Admin',
}

export interface NavEntry {
  /** The route name, used to keep a parent item current on a child route. */
  name?: string
  path: string
  label: string
  group: NavGroup
  groupLabel: string
  order: number
}

export interface NavSection {
  group: NavGroup
  label: string
  items: NavEntry[]
}

/** A role string is accepted as well as a list, for callers with one role. */
type Roles = readonly string[] | string

function asRoles(roles: Roles): readonly string[] {
  return typeof roles === 'string' ? [roles] : roles
}

function isNavGroup(value: unknown): value is NavGroup {
  return value === 'operate' || value === 'content' || value === 'admin'
}

/**
 * The visible navigation, grouped and ordered.
 *
 * A route is included when it sets `meta.nav`, declares a known `navGroup`, and
 * its `meta.minRole` is met. A missing `minRole` is treated as `viewer` so a
 * malformed route is hidden from the least privileged session rather than shown
 * to everyone; the guard is still the authority on access either way.
 *
 * Groups with no items are dropped, so an empty Admin section is not rendered
 * before the tickets that add its routes exist.
 */
export function navGroups(routes: readonly RouteRecordRaw[], roles: Roles): NavSection[] {
  const held = asRoles(roles)
  const sections: NavSection[] = NAV_GROUP_ORDER.map((group) => ({
    group,
    label: NAV_GROUP_LABELS[group],
    items: [],
  }))
  const byGroup = new Map(sections.map((section) => [section.group, section]))

  for (const route of routes) {
    const meta = route.meta
    if (meta?.nav !== true || !isNavGroup(meta.navGroup)) continue
    // A nav item is a link to a fixed page; a parameterised route is reached
    // from its parent instead and would render a literal `:` in the menu.
    if (typeof route.path !== 'string' || route.path.includes(':')) continue

    const minRole = typeof meta.minRole === 'string' ? meta.minRole : 'viewer'
    if (!hasRoleAtLeast(held, minRole)) continue

    const section = byGroup.get(meta.navGroup)
    if (section === undefined) continue
    const label = typeof meta.title === 'string' && meta.title.length > 0 ? meta.title : route.path
    section.items.push({
      name: typeof route.name === 'string' ? route.name : undefined,
      path: route.path,
      label,
      group: meta.navGroup,
      groupLabel: NAV_GROUP_LABELS[meta.navGroup],
      order: typeof meta.navOrder === 'number' ? meta.navOrder : Number.MAX_SAFE_INTEGER,
    })
  }

  for (const section of sections) {
    // Stable, so two routes with no explicit order keep their table order.
    section.items.sort((a, b) => a.order - b.order)
  }
  return sections.filter((section) => section.items.length > 0)
}

export interface Crumb {
  label: string
  /** Where the crumb links, or null for the current page. */
  to: string | null
}

/** One palette row: a page the command palette may jump to. */
export interface PaletteEntry {
  path: string
  label: string
  /** The section shown at the right, e.g. `Operate` or `Account`. */
  groupLabel: string
}

/**
 * Every page the palette offers: the same role-filtered navigation the sidebar
 * shows, plus routes marked `meta.palette` that are deliberately not in a nav
 * group (the account page is reached from the sidebar footer, not the menu).
 *
 * Sharing `navGroups` here is what keeps the two entry points from disagreeing
 * about who may see what.
 */
export function paletteEntries(routes: readonly RouteRecordRaw[], roles: Roles): PaletteEntry[] {
  const held = asRoles(roles)
  const entries: PaletteEntry[] = navGroups(routes, held).flatMap((section) =>
    section.items.map((item) => ({
      path: item.path,
      label: item.label,
      groupLabel: item.groupLabel,
    })),
  )

  for (const route of routes) {
    if (route.meta?.palette !== true) continue
    if (typeof route.path !== 'string' || route.path.includes(':')) continue
    const minRole = typeof route.meta.minRole === 'string' ? route.meta.minRole : 'viewer'
    if (!hasRoleAtLeast(held, minRole)) continue
    const label =
      typeof route.meta.title === 'string' && route.meta.title.length > 0
        ? route.meta.title
        : route.path
    entries.push({ path: route.path, label, groupLabel: 'Account' })
  }

  return entries
}

/**
 * The minimum a breadcrumb needs from a route, so a test can pass a literal
 * instead of a router.
 */
export interface BreadcrumbRoute {
  name?: unknown
  params?: Record<string, string | string[] | undefined>
}

function findRecord(routes: readonly RouteRecordRaw[], name: string): RouteRecordRaw | undefined {
  return routes.find((route) => route.name === name)
}

function parameterNames(path: string): string[] {
  return [...path.matchAll(/:([A-Za-z0-9_]+)/g)].map((match) => match[1])
}

/**
 * A record's label: the value of its dynamic segment when there is one (the
 * namespace editor is per-namespace and its `meta.title` alone would say
 * nothing), otherwise `meta.title`.
 */
function labelFor(
  record: RouteRecordRaw,
  params: Record<string, string | string[] | undefined>,
): string {
  for (const name of parameterNames(record.path)) {
    const value = params[name]
    if (typeof value === 'string' && value.length > 0) return value
  }
  if (typeof record.meta?.title === 'string' && record.meta.title.length > 0) {
    return record.meta.title
  }
  return typeof record.name === 'string' ? record.name : record.path
}

/**
 * The chain of crumbs for a route, walked through `meta.crumbParent` and
 * returned root-first. The last crumb is the current page and carries no link.
 */
export function breadcrumbs(route: BreadcrumbRoute, routes: readonly RouteRecordRaw[]): Crumb[] {
  const name = typeof route.name === 'string' ? route.name : null
  if (name === null) return []

  const chain: RouteRecordRaw[] = []
  const seen = new Set<string>()
  let current: string | null = name
  while (current !== null && !seen.has(current)) {
    seen.add(current)
    const record = findRecord(routes, current)
    if (record === undefined) break
    chain.unshift(record)
    const parent = record.meta?.crumbParent
    current = typeof parent === 'string' ? parent : null
  }

  const params = route.params ?? {}
  return chain.map((record, index) => {
    const isLast = index === chain.length - 1
    return {
      label: labelFor(record, isLast ? params : {}),
      to: isLast ? null : record.path,
    }
  })
}
