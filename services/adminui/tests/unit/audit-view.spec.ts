import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { http, HttpResponse } from 'msw'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { describe, expect, it } from 'vitest'

import { server } from '@/mocks/server'
import AuditView from '@/views/dashboard/AuditView.vue'

/**
 * The audit page's own logic: the URL is the filter state, "load more" appends,
 * a row expands to its details JSON, a degraded source is named, and a release
 * row links to the diff of the release it published. Ionic is stubbed; the
 * browser behaviour is the e2e suite's job.
 */

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonSpinner: true,
}

async function mountView(query = ''): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/dashboard/audit', name: 'dashboard-audit', component: AuditView },
      { path: '/dashboard', name: 'dashboard-overview', component: { template: '<div />' } },
      { path: '/config', name: 'config-namespaces', component: { template: '<div />' } },
      {
        path: '/config/namespaces/:namespace',
        name: 'config-namespace-editor',
        component: { template: '<div />' },
      },
      {
        path: '/config/releases/:channel/:releaseId',
        name: 'config-release-diff',
        component: { template: '<div />' },
      },
    ],
  })
  await router.push(`/dashboard/audit${query}`)
  await router.isReady()

  const wrapper = mount(AuditView, {
    global: {
      plugins: [router],
      stubs: IONIC_STUBS,
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  await flushPromises()
  return { wrapper, router }
}

function rows(wrapper: VueWrapper) {
  return wrapper.findAll('.data-table__row')
}

function rowWithText(wrapper: VueWrapper, text: string) {
  const row = rows(wrapper).find((candidate) => candidate.text().includes(text))
  expect(row).toBeDefined()
  return row!
}

describe('AuditView', () => {
  it('writes a filter change to the URL, which is the state it keeps', async () => {
    const { wrapper, router } = await mountView()

    await wrapper.find('[data-testid="audit-filter-source"]').setValue('config')
    await flushPromises()

    expect(router.currentRoute.value.query.source).toBe('config')

    await wrapper.find('[data-testid="audit-filter-actor"]').setValue('Liveops')
    await flushPromises()
    expect(router.currentRoute.value.query.actor).toBe('Liveops')
    wrapper.unmount()
  })

  it('reads the filters back out of the URL on the way in', async () => {
    const { wrapper } = await mountView(
      '?source=config&actor=Liveops&from=2026-09-01T00:00&to=2026-09-30T00:00',
    )

    expect(
      wrapper.find<HTMLSelectElement>('[data-testid="audit-filter-source"]').element.value,
    ).toBe('config')
    expect(wrapper.find<HTMLInputElement>('[data-testid="audit-filter-actor"]').element.value).toBe(
      'Liveops',
    )
    expect(wrapper.find<HTMLInputElement>('[data-testid="audit-filter-from"]').element.value).toBe(
      '2026-09-01T00:00',
    )
    expect(wrapper.find<HTMLInputElement>('[data-testid="audit-filter-to"]').element.value).toBe(
      '2026-09-30T00:00',
    )
    wrapper.unmount()
  })

  it('appends the next page when Load more is clicked', async () => {
    const { wrapper } = await mountView()

    expect(rows(wrapper)).toHaveLength(5)

    await wrapper.get('[data-testid="audit-load-more"]').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(rows(wrapper)).toHaveLength(10)
    // The cursor was the last page, so the button is gone rather than looping.
    expect(wrapper.find('[data-testid="audit-load-more"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('expands a row to its pretty-printed details JSON', async () => {
    const { wrapper } = await mountView()
    const row = rowWithText(wrapper, 'draft.save')

    expect(row.find('[data-testid="audit-details"]').exists()).toBe(false)
    await row.get('[data-testid="audit-expand"]').trigger('click')

    const details = row.get('[data-testid="audit-details"]')
    expect(details.text()).toContain('"revision"')
    expect(details.text()).toContain('12')
    expect(details.text()).toContain('\n')
    wrapper.unmount()
  })

  it('names the source the merge could not read instead of showing a whole trail', async () => {
    server.use(
      http.get('*/api/admin/dashboard/audit', () =>
        HttpResponse.json({ entries: [], next_cursor: null, degraded: ['admin-auth'] }),
      ),
    )

    const { wrapper } = await mountView()
    const notice = wrapper.get('[data-testid="audit-degraded"]')

    expect(notice.text()).toContain('admin-auth')
    expect(notice.text()).toContain('incomplete')
    wrapper.unmount()
  })

  it('links a release.publish row to the diff of that release against its base', async () => {
    const { wrapper } = await mountView()
    const row = rowWithText(wrapper, 'release.publish')

    const link = row.get('[data-testid="audit-changes-link"]')
    expect(link.text()).toContain('View changes')
    expect(link.attributes('href')).toBe('/config/releases/dev/3?base=2')

    // A release.rollback row carries `to`/`from` rather than release/base ids
    // and must link the same way.
    const rollback = rowWithText(wrapper, 'release.rollback')
    expect(rollback.get('[data-testid="audit-changes-link"]').attributes('href')).toBe(
      '/config/releases/dev/2?base=3',
    )
    wrapper.unmount()
  })
})
