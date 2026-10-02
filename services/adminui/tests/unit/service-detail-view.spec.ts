import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { defineComponent } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import * as dashboardApi from '@/api/dashboard'
import ServiceDetailView from '@/views/dashboard/ServiceDetailView.vue'

/**
 * The service detail page's own logic, mounted against the fixture API.
 *
 * The chart itself is a canvas and is exercised end to end; here it is a stub
 * that records the series it was handed, which is what makes "one chart per
 * template" and "a failed chart does not take the rest down" assertable without
 * a browser.
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

const ChartStub = defineComponent({
  name: 'TimeSeriesChart',
  props: {
    t: { type: Array, required: true },
    series: { type: Array, required: true },
    unit: { type: String, required: true },
    height: { type: Number, default: 200 },
  },
  template: `
    <div class="chart-stub" :data-unit="unit">
      <span v-for="column in series" :key="column.name" class="chart-stub__series">
        {{ column.name }}
      </span>
    </div>
  `,
})

async function mountView() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/dashboard', name: 'dashboard-overview', component: { template: '<div />' } },
      {
        path: '/dashboard/services/:name',
        name: 'dashboard-service-detail',
        component: { template: '<div />' },
      },
    ],
  })
  await router.push('/dashboard/services/config?range=1h')
  await router.isReady()

  const wrapper = mount(ServiceDetailView, {
    global: {
      plugins: [router],
      stubs: { ...IONIC_STUBS, TimeSeriesChart: ChartStub },
      renderStubDefaultSlot: true,
    },
  })
  for (let index = 0; index < 4; index += 1) await flushPromises()
  return wrapper
}

function chartSeries(wrapper: VueWrapper): string[] {
  return wrapper.findAll('.chart-stub__series').map((node) => node.text())
}

function panelByTitle(wrapper: VueWrapper, title: string) {
  const panel = wrapper
    .findAll('.panel')
    .find((candidate) => candidate.find('.panel__title').text() === title)
  expect(panel).toBeDefined()
  return panel!
}

describe('ServiceDetailView', () => {
  it('renders a chart for every template, latency sharing one chart', async () => {
    const wrapper = await mountView()

    expect(wrapper.findAll('.chart-stub')).toHaveLength(8)
    expect(new Set(chartSeries(wrapper))).toEqual(
      new Set([
        '2xx',
        '3xx',
        '4xx',
        '5xx',
        'p50',
        'p95',
        'p99',
        'error_ratio',
        'cpu',
        'memory',
        'goroutines',
        'heap_inuse',
        'gc_pause_max',
      ]),
    )

    // Latency is three templates drawn as three series on one canvas.
    const latency = panelByTitle(wrapper, 'Latency')
    expect(latency.findAll('.chart-stub')).toHaveLength(1)
    expect(
      latency
        .findAll('.chart-stub__series')
        .map((node) => node.text())
        .sort(),
    ).toEqual(['p50', 'p95', 'p99'])

    wrapper.unmount()
  })

  it('names the service in the title and shows the range default', async () => {
    const wrapper = await mountView()

    expect(wrapper.find('h1').text()).toBe('config')
    expect(wrapper.find('[data-testid="range-1h"]').attributes('data-selected')).toBe('true')
    expect(wrapper.find('[data-testid="range-24h"]').attributes('data-selected')).toBe('false')

    wrapper.unmount()
  })

  it('shows a failed template in its own panel and leaves the rest rendering', async () => {
    // The real reader is kept for every other template; only the latency p95
    // request fails, which is the one-chart-down case the page has to absorb.
    const realGetSeries = dashboardApi.getSeries
    vi.spyOn(dashboardApi, 'getSeries').mockImplementation(
      async (service, metric, query, signal) => {
        if (metric === 'latency_p95') throw new Error('query failed')
        return realGetSeries(service, metric, query, signal)
      },
    )

    const wrapper = await mountView()

    const latency = panelByTitle(wrapper, 'Latency')
    expect(latency.find('.chart-stub').exists()).toBe(false)
    expect(latency.find('.error-panel').text()).toContain('query failed')
    expect(latency.find('.error-panel__retry').exists()).toBe(true)

    // Every other chart still rendered.
    expect(wrapper.findAll('.chart-stub')).toHaveLength(7)
    expect(chartSeries(wrapper)).toContain('2xx')
    expect(chartSeries(wrapper)).toContain('cpu')

    wrapper.unmount()
  })
})
