import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'

import LogsView from '@/views/dashboard/LogsView.vue'

/**
 * The log explorer's own logic, with Ionic stubbed and the virtualiser replaced
 * by one that renders every row. The page's job is the filters and the row's
 * expansion behaviour, not the browser layout a real virtualiser needs; the e2e
 * suite is where that is exercised.
 */

vi.mock('@tanstack/vue-virtual', async () => {
  const { computed, unref } = await import('vue')
  return {
    useVirtualizer: (options: unknown) => {
      return computed(() => {
        const resolved = unref(options) as {
          count: number
          getItemKey?: (index: number) => unknown
        }
        return {
          getVirtualItems: () =>
            Array.from({ length: resolved.count }, (_value, index) => ({
              index,
              key: resolved.getItemKey ? resolved.getItemKey(index) : index,
              start: index * 44,
              size: 44,
            })),
          getTotalSize: () => resolved.count * 44,
          measure: () => undefined,
          measureElement: () => undefined,
        }
      })
    },
  }
})

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
      { path: '/dashboard/logs', name: 'dashboard-logs', component: LogsView },
      { path: '/dashboard', name: 'dashboard-overview', component: { template: '<div />' } },
    ],
  })
  await router.push(`/dashboard/logs${query}`)
  await router.isReady()

  const wrapper = mount(LogsView, {
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

function rowWithText(wrapper: VueWrapper, text: string) {
  const row = wrapper
    .findAll('[data-testid="log-row"]')
    .find((candidate) => candidate.text().includes(text))
  expect(row).toBeDefined()
  return row!
}

describe('LogsView', () => {
  it('renders the fixture lines once the history request answers', async () => {
    const { wrapper } = await mountView()
    expect(wrapper.findAll('[data-testid="log-row"]').length).toBeGreaterThan(0)
    expect(wrapper.text()).toContain('gateway_dev')
    wrapper.unmount()
  })

  it('writes a filter change to the URL, which is the only state it keeps', async () => {
    const { wrapper, router } = await mountView()

    await wrapper.find('[data-testid="filter-level"]').setValue('error')
    await flushPromises()

    expect(router.currentRoute.value.query.level).toBe('error')
    wrapper.unmount()
  })

  it('reads the filters back out of the URL on the way in', async () => {
    const { wrapper } = await mountView('?level=warn&service=config&request_id=req_x')

    const level = wrapper.find<HTMLSelectElement>('[data-testid="filter-level"]').element
    const service = wrapper.find<HTMLInputElement>('[data-testid="filter-service"]').element
    const requestId = wrapper.find<HTMLInputElement>('[data-testid="filter-request-id"]').element
    expect(level.value).toBe('warn')
    expect(service.value).toBe('config')
    expect(requestId.value).toBe('req_x')
    wrapper.unmount()
  })

  it('expands a row to show its pretty-printed fields JSON', async () => {
    const { wrapper } = await mountView()

    const row = rowWithText(wrapper, 'request route=')
    expect(row.find('[data-testid="log-fields"]').exists()).toBe(false)

    await row.find('.logs__summary').trigger('click')

    const fields = row.find('[data-testid="log-fields"]')
    expect(fields.exists()).toBe(true)
    expect(fields.text()).toContain('"request_id"')
    expect(fields.text()).toContain('req_0000000000a1')
    wrapper.unmount()
  })

  it('follows a request id by setting the filter, since that is how a trace is chased', async () => {
    const { wrapper, router } = await mountView()

    const row = rowWithText(wrapper, 'request route=')
    await row.find('.logs__summary').trigger('click')
    await row.find('[data-testid="log-request-link"]').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.query.request_id).toBe('req_0000000000a1')
    wrapper.unmount()
  })

  it('debounces the contains field before touching the URL', async () => {
    const { wrapper, router } = await mountView()
    const input = wrapper.find('[data-testid="filter-contains"]')

    await input.setValue('needle')
    expect(router.currentRoute.value.query.contains).toBeUndefined()

    await new Promise((resolve) => setTimeout(resolve, 350))
    await flushPromises()
    expect(router.currentRoute.value.query.contains).toBe('needle')
    wrapper.unmount()
  })
})

describe('LogsView load more', () => {
  it('keeps lines that share the oldest loaded instant but missed the first page', async () => {
    const { http, HttpResponse } = await import('msw')
    const { server } = await import('@/mocks/server')

    // 510 lines, newest first. The last 30 share one millisecond (distinct
    // nanoseconds), and the 500-line page boundary falls inside that group.
    const lines = Array.from({ length: 510 }, (_, i) => {
      const at =
        i < 480
          ? new Date(Date.UTC(2026, 8, 26, 10, 0, 0) + (510 - i) * 1000).toISOString()
          : `2026-09-26T10:00:00.123${String(999999 - i).padStart(6, '0')}Z`
      return { at, service: 'config', level: 'info', message: `line ${i}` }
    })
    server.use(
      http.get('*/api/admin/dashboard/logs', ({ request }) => {
        const url = new URL(request.url)
        const to = url.searchParams.get('to')
        const limit = Number(url.searchParams.get('limit') ?? '500')
        // RFC3339 strings of one width compare correctly as text; `to` is inclusive.
        const hits = lines.filter((line) => to === null || line.at <= to)
        return HttpResponse.json({ entries: hits.slice(0, limit) })
      }),
    )

    const { wrapper } = await mountView('?range=24h')
    await wrapper.get('[data-testid="load-more"]').trigger('click')
    await flushPromises()
    await flushPromises()

    const held = (wrapper.vm as unknown as { history: unknown[] }).history
    expect(held).toHaveLength(510)
  })
})
