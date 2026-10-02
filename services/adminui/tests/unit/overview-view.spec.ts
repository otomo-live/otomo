import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { HttpResponse, http } from 'msw'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'

import dashboardOverviewFixture from '@/mocks/fixtures/dashboard-overview.json'
import { server } from '@/mocks/server'
import OverviewView from '@/views/dashboard/OverviewView.vue'

/**
 * The redesigned overview, mounted against the fixture API.
 *
 * Ionic is auto-stubbed: these tests are about the page's own logic and markup,
 * and the real components would drag a shadow-DOM layout in for no extra
 * coverage. The router is real so `useRouter`/`RouterLink` resolve, and the
 * shared fixture from `src/mocks/handlers.ts` is what the page reads.
 */

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonButton: true,
  IonSpinner: true,
}

async function mountView() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/dashboard', name: 'dashboard-overview', component: { template: '<div />' } },
    ],
  })
  await router.push('/dashboard')
  await router.isReady()

  const wrapper = mount(OverviewView, {
    global: {
      plugins: [router],
      stubs: IONIC_STUBS,
      // The stubs stand in for Ionic, they do not hide the page: without this
      // they swallow the default slots the whole view is nested in.
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  await flushPromises()
  return wrapper
}

function cardNames(wrapper: VueWrapper): string[] {
  return wrapper
    .findAll('[data-testid="service-card"]')
    .map((card) => card.find('.overview__card-name').text())
}

function cardByName(wrapper: VueWrapper, name: string) {
  const card = wrapper
    .findAll('[data-testid="service-card"]')
    .find((candidate) => candidate.find('.overview__card-name').text() === name)
  expect(card).toBeDefined()
  return card!
}

/** Serve the default overview with one or more fields replaced. */
function overviewWith(overrides: Record<string, unknown>): void {
  server.use(
    http.get('/api/admin/dashboard/overview', () =>
      HttpResponse.json({
        ...dashboardOverviewFixture,
        generated_at: new Date().toISOString(),
        ...overrides,
      }),
    ),
  )
}

const CRITICAL_AND_WARNING = [
  {
    name: 'PlayerServiceDown',
    severity: 'critical',
    service: 'session',
    summary: 'session is down',
    active_at: '2026-09-29T03:10:00Z',
  },
  {
    name: 'GameServerPoolEmpty',
    severity: 'warning',
    service: '',
    summary: 'Allocator game-server pool is empty',
    active_at: '2026-09-29T02:00:00Z',
  },
]

describe('OverviewView', () => {
  it('sorts not-up first, then not-ready, then by name', async () => {
    const wrapper = await mountView()
    // patch is down, admin-auth is up but not ready, the rest are healthy.
    expect(cardNames(wrapper)).toEqual([
      'patch',
      'admin-auth',
      'config',
      'dashboard',
      'gateway_dev',
    ])
    wrapper.unmount()
  })

  it('renders a null figure as an em dash with an unavailable hint, never zero', async () => {
    const wrapper = await mountView()

    const disk = wrapper.findAll('.stat-tile').find((tile) => tile.text().includes('Disk'))
    expect(disk).toBeDefined()
    expect(disk!.text()).toContain('—')
    expect(disk!.text()).toContain('unavailable')

    const gateway = cardByName(wrapper, 'gateway_dev')
    expect(gateway.text()).toContain('p95 — ms')
    wrapper.unmount()
  })

  it('shows the ready reason on a not-ready card', async () => {
    const wrapper = await mountView()

    const adminAuth = cardByName(wrapper, 'admin-auth')
    expect(adminAuth.find('.overview__reason').text()).toContain('connection refused')
    // A healthy card has no reason line at all.
    expect(cardByName(wrapper, 'config').find('.overview__reason').exists()).toBe(false)
    wrapper.unmount()
  })

  it('names the unavailable figures at the top rather than blanking the page', async () => {
    const wrapper = await mountView()

    expect(wrapper.text()).toContain('Some figures are unavailable right now: disk')
    // The cards still render underneath the notice.
    expect(cardNames(wrapper)).toHaveLength(5)
    wrapper.unmount()
  })

  it('leaves a card unlinked while the service detail route does not exist', async () => {
    const wrapper = await mountView()
    expect(wrapper.find('[data-testid="service-card"]').element.tagName).toBe('ARTICLE')
    wrapper.unmount()
  })

  it('shows no alerts panel when nothing is firing', async () => {
    overviewWith({ alerts: [] })
    const wrapper = await mountView()
    expect(wrapper.find('[data-testid="overview-alerts"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('renders the critical alert first with badges, summary, service and the alert role', async () => {
    overviewWith({ alerts: CRITICAL_AND_WARNING })
    const wrapper = await mountView()

    const panel = wrapper.find('[data-testid="overview-alerts"]')
    expect(panel.exists()).toBe(true)
    expect(panel.attributes('role')).toBe('alert')
    expect(panel.text()).toContain('Firing alerts (2)')

    const rows = wrapper.findAll('[data-testid="overview-alert"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('.overview__alert-badge').text()).toBe('critical')
    expect(rows[0].text()).toContain('session is down')
    expect(rows[0].text()).toContain('session')
    expect(rows[0].text()).toContain('since')
    expect(rows[1].find('.overview__alert-badge').text()).toBe('warning')
    expect(rows[1].text()).toContain('Allocator game-server pool is empty')
    wrapper.unmount()
  })

  it('uses the status role when only a warning is firing', async () => {
    overviewWith({ alerts: [CRITICAL_AND_WARNING[1]] })
    const wrapper = await mountView()

    const panel = wrapper.find('[data-testid="overview-alerts"]')
    expect(panel.attributes('role')).toBe('status')
    wrapper.unmount()
  })

  it('names alerts in the degraded notice', async () => {
    overviewWith({ alerts: [], degraded: ['host.disk', 'alerts'] })
    const wrapper = await mountView()
    expect(wrapper.text()).toContain('Some figures are unavailable right now: disk, alerts')
    wrapper.unmount()
  })
})
