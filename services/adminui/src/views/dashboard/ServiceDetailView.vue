<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonMenuButton,
  IonPage,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { getSeries, type SeriesResult } from '@/api/dashboard'
import { describeFailure, type FailureMessage } from '@/api/messages'
import {
  isPollingRange,
  parseRangeKey,
  parseTo,
  rangeByKey,
  rangeQuery,
  resolveWindow,
  SERIES_RANGES,
  type SeriesWindow,
} from '@/charts/range'
import TimeSeriesChart from '@/components/charts/TimeSeriesChart.vue'
import { Breadcrumbs, ErrorPanel, Grid, PageHeader, Panel, Toolbar } from '@/components/ui'
import { usePolling } from '@/composables/usePolling'

/**
 * One service's own page: the templates the detail endpoint offers, drawn as
 * uPlot charts.
 *
 * The range lives in the URL (`?range=6h`, `?to=<unix>`), not in component
 * state, so a link is shareable and a reload rebuilds the same picture. Every
 * template is fetched concurrently: they share a window and a failure in one
 * must not blank the rest, so each panel owns its own error and retry.
 *
 * A short range polls every 30s through `usePolling`, which already holds no
 * timer while the tab is hidden. A 24h/7d window moves too slowly to be worth
 * the request, and a pinned `to` is fixed by definition, so neither polls.
 */

interface ChartSpec {
  key: string
  /** The `Panel` this chart renders inside; several charts may share one. */
  panel: string
  title: string
  unit: string
  templates: string[]
}

/**
 * The templates to draw, grouped exactly as the page presents them. Latency is
 * three templates on one chart; the Go runtime figures have incompatible units
 * (count, bytes, seconds) and so get a chart each inside one panel.
 */
const CHART_SPECS: readonly ChartSpec[] = [
  {
    key: 'traffic',
    panel: 'Traffic',
    title: 'Traffic',
    unit: 'req/s',
    templates: ['rps_by_status'],
  },
  {
    key: 'latency',
    panel: 'Latency',
    title: 'Latency',
    unit: 'ms',
    templates: ['latency_p50', 'latency_p95', 'latency_p99'],
  },
  {
    key: 'error_ratio',
    panel: 'Error ratio',
    title: 'Error ratio',
    unit: '%',
    templates: ['error_ratio'],
  },
  { key: 'cpu', panel: 'CPU', title: 'CPU', unit: 'cores', templates: ['cpu'] },
  { key: 'memory', panel: 'Memory', title: 'Memory', unit: 'bytes', templates: ['memory'] },
  {
    key: 'goroutines',
    panel: 'Go runtime',
    title: 'Goroutines',
    unit: 'count',
    templates: ['goroutines'],
  },
  {
    key: 'heap_inuse',
    panel: 'Go runtime',
    title: 'Heap in use',
    unit: 'bytes',
    templates: ['heap_inuse'],
  },
  {
    key: 'gc_pause_max',
    panel: 'Go runtime',
    title: 'GC pause max',
    unit: 's',
    templates: ['gc_pause_max'],
  },
]

const PANEL_ORDER = ['Traffic', 'Latency', 'Error ratio', 'CPU', 'Memory', 'Go runtime']

const ALL_TEMPLATES: string[] = [...new Set(CHART_SPECS.flatMap((chart) => chart.templates))]

const POLL_INTERVAL_MS = 30_000

interface ChartView extends ChartSpec {
  data: { t: number[]; series: { name: string; values: (number | null)[] }[] } | null
  error: FailureMessage | null
}

const route = useRoute()
const router = useRouter()

const results = ref<Record<string, SeriesResult | null>>({})
const errors = ref<Record<string, FailureMessage | null>>({})

const serviceName = computed(() => {
  const name = route.params.name
  return typeof name === 'string' ? name : ''
})

const rangeKey = computed(() => parseRangeKey(route.query.range))
const range = computed(() => rangeByKey(rangeKey.value))
const pinnedTo = computed(() => parseTo(route.query.to))

function currentWindow(): SeriesWindow {
  return resolveWindow(range.value, Date.now(), pinnedTo.value)
}

function setResult(
  template: string,
  result: SeriesResult | null,
  error: FailureMessage | null,
): void {
  results.value = { ...results.value, [template]: result }
  errors.value = { ...errors.value, [template]: error }
}

async function fetchTemplate(
  template: string,
  window: SeriesWindow,
  signal?: AbortSignal,
): Promise<void> {
  if (serviceName.value === '') return
  try {
    const result = await getSeries(
      serviceName.value,
      template,
      { from: window.from, to: window.to, step: window.step },
      signal,
    )
    setResult(template, result, null)
  } catch (error) {
    // A hide/unmount or a range change aborts the request; that is not a
    // failure to draw attention to.
    if (signal?.aborted === true) return
    setResult(template, null, describeFailure(error))
  }
}

async function load(signal?: AbortSignal): Promise<void> {
  const window = currentWindow()
  await Promise.all(ALL_TEMPLATES.map((template) => fetchTemplate(template, window, signal)))
}

function toChartView(spec: ChartSpec): ChartView {
  const error =
    spec.templates
      .map((template) => errors.value[template])
      .find((failure): failure is FailureMessage => failure != null) ?? null
  const loaded = spec.templates
    .map((template) => results.value[template])
    .filter((result): result is SeriesResult => result != null)
  const data =
    error === null && loaded.length === spec.templates.length && loaded[0] !== undefined
      ? { t: loaded[0].t, series: loaded.flatMap((result) => result.series) }
      : null
  return { ...spec, data, error }
}

const charts = computed<ChartView[]>(() => CHART_SPECS.map(toChartView))

const panels = computed(() =>
  PANEL_ORDER.map((title) => ({
    title,
    charts: charts.value.filter((chart) => chart.panel === title),
  })).filter((panel) => panel.charts.length > 0),
)

/** `clamped` is the service saying it narrowed the window; the page must too. */
const clamped = computed(() =>
  Object.values(results.value).some((result) => result?.clamped === true),
)

const polling = usePolling({ fn: (signal) => load(signal), intervalMs: POLL_INTERVAL_MS })

function shouldPoll(): boolean {
  return isPollingRange(range.value) && pinnedTo.value === undefined
}

function syncPolling(): void {
  if (shouldPoll()) polling.start()
  else polling.stop()
}

function selectRange(key: string): void {
  if (key === rangeKey.value) return
  // `replace` rather than `push`: the range is a view of the page's state, and
  // flooding the back stack with every click would make Back unusable.
  void router.replace({ query: rangeQuery(key, pinnedTo.value) })
}

function retry(chart: ChartView): void {
  const window = currentWindow()
  for (const template of chart.templates) setResult(template, results.value[template], null)
  void Promise.all(chart.templates.map((template) => fetchTemplate(template, window)))
}

// Auto-starts the poll on mount. A long or pinned range stops that poll and
// loads once by hand: `stop()` aborts the in-flight first poll, so the initial
// read has to be issued again after it rather than left to the aborted one.
onMounted(() => {
  if (!shouldPoll()) {
    polling.stop()
    void load()
  }
})

watch([rangeKey, pinnedTo], () => {
  void load()
  syncPolling()
})
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page">
        <PageHeader :title="serviceName">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <Toolbar>
              <IonButton
                v-for="candidate in SERIES_RANGES"
                :key="candidate.key"
                size="small"
                :fill="candidate.key === rangeKey ? 'solid' : 'clear'"
                :data-testid="`range-${candidate.key}`"
                :data-selected="candidate.key === rangeKey ? 'true' : 'false'"
                @click="selectRange(candidate.key)"
              >
                {{ candidate.label }}
              </IonButton>
            </Toolbar>
          </template>
        </PageHeader>

        <p v-if="clamped" class="service__clamped" role="status">showing the last 7 days</p>

        <Grid :min="420" :gap="4">
          <Panel v-for="panel in panels" :key="panel.title" :title="panel.title">
            <div class="service__charts">
              <div v-for="chart in panel.charts" :key="chart.key" class="service__chart">
                <h3 v-if="panel.charts.length > 1" class="service__chart-title">
                  {{ chart.title }}
                </h3>
                <TimeSeriesChart
                  v-if="chart.data !== null"
                  :t="chart.data.t"
                  :series="chart.data.series"
                  :unit="chart.unit"
                  :height="220"
                />
                <ErrorPanel
                  v-else-if="chart.error !== null"
                  :message="chart.error.summary"
                  :detail="chart.error.detail"
                  :request-id="chart.error.reference"
                  retry-label="Retry"
                  @retry="retry(chart)"
                />
                <p v-else class="service__chart-loading">
                  <IonSpinner name="dots" />
                  Loading...
                </p>
              </div>
            </div>
          </Panel>
        </Grid>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.service__clamped {
  background: var(--ds-info-soft);
  border: 1px solid var(--ds-info);
  border-radius: var(--ds-radius-md);
  color: var(--ds-text);
  margin: 0 0 var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.service__charts {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-4);
}

.service__chart-title {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
  margin: 0 0 var(--ds-space-1);
}

.service__chart-loading {
  align-items: center;
  color: var(--ds-text-muted);
  display: flex;
  gap: var(--ds-space-2);
  margin: 0;
  min-height: 220px;
}
</style>
