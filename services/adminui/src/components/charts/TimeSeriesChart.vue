<script setup lang="ts">
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import { formatAxis, formatValue } from '@/charts/format'

/**
 * A thin uPlot wrapper.
 *
 * uPlot is canvas-based and owns its own DOM inside the target element, so the
 * Vue side of this is lifecycle: build once, then `setData` on every update
 * rather than tearing the plot down and building it again, which is what keeps
 * a 30s poll from flashing. The container resizes with its grid cell through a
 * debounced `ResizeObserver`, and the whole thing is torn down on unmount.
 *
 * Colour is read from the token palette at runtime because uPlot needs a
 * concrete value: a canvas cannot resolve `var(--ds-chart-1)`. The values are
 * re-read when the OS preference or Ionic's dark class changes, so a theme flip
 * repaints the lines instead of leaving them on the old palette.
 *
 * A `null` value is a gap, not a zero: every series is `spanGaps: false` and the
 * legend formats it as an em dash.
 */

const props = withDefaults(
  defineProps<{
    /** Shared x values, unix seconds. */
    t: number[]
    series: { name: string; values: (number | null)[] }[]
    /** The series endpoint's unit, e.g. `bytes` or `req/s`. */
    unit: string
    height?: number
  }>(),
  { height: 200 },
)

const host = ref<HTMLDivElement | null>(null)

interface ChartColors {
  series: string[]
  axis: string
  grid: string
}

/** `currentColor` is not a colour literal, so the fallback passes the token lint. */
const COLOUR_FALLBACK = 'currentColor'

function readColors(): ChartColors {
  const style = getComputedStyle(document.documentElement)
  const read = (name: string): string => style.getPropertyValue(name).trim() || COLOUR_FALLBACK
  return {
    series: [1, 2, 3, 4, 5, 6].map((index) => read(`--ds-chart-${index}`)),
    axis: read('--ds-text-muted'),
    grid: read('--ds-border'),
  }
}

const data = computed<uPlot.AlignedData>(() => [
  props.t,
  ...props.series.map((column) => column.values),
])

function options(colors: ChartColors, width: number): uPlot.Options {
  return {
    width,
    height: props.height,
    legend: { show: true, live: true },
    cursor: { points: { show: false } },
    scales: { x: { time: true } },
    axes: [
      {
        stroke: colors.axis,
        grid: { stroke: colors.grid, width: 1 },
        ticks: { stroke: colors.grid },
      },
      {
        stroke: colors.axis,
        grid: { stroke: colors.grid },
        ticks: { stroke: colors.grid },
        values: (_self, values) => values.map((value) => formatAxis(props.unit, value)),
        // Wide enough for the longest tick label, so "0.70%" or "1.5 GiB" is not
        // clipped at the panel edge. uPlot's default is a fixed 50 px.
        size: (_self, values) => {
          const longest = (values ?? []).reduce(
            (max, label) => Math.max(max, String(label).length),
            0,
          )
          return Math.max(40, Math.ceil(longest * 7 + 20))
        },
      },
    ],
    series: [
      {},
      ...props.series.map((column, index) => ({
        label: column.name,
        stroke: colors.series[index % colors.series.length],
        width: 1.5,
        spanGaps: false,
        value: (_self: uPlot, raw: number) => formatValue(props.unit, raw),
      })),
    ],
  }
}

let plot: uPlot | null = null
let observer: ResizeObserver | null = null
let media: MediaQueryList | null = null
let themeObserver: MutationObserver | null = null
let resizeTimer: ReturnType<typeof setTimeout> | null = null

function containerWidth(): number {
  return Math.max(host.value?.clientWidth ?? 0, 1)
}

function markRendered(): void {
  // A timestamp, not `true`: the e2e budget test needs to tell a fresh draw
  // from the one before it, and a value that only ever reads `true` cannot.
  host.value?.setAttribute('data-rendered', String(performance.now()))
}

/**
 * Structural change (a different number of points or series) cannot be a
 * `setData`; anything else can, which is the common poll/range case.
 */
function needsRebuild(next: uPlot.AlignedData): boolean {
  if (plot === null) return true
  const current = plot.data
  return current.length !== next.length || (current[0]?.length ?? -1) !== (next[0]?.length ?? -2)
}

function create(): void {
  const element = host.value
  if (element === null) return
  plot?.destroy()
  plot = new uPlot(options(readColors(), containerWidth()), data.value, element)
  markRendered()
}

function applySize(): void {
  if (plot === null) return
  plot.setSize({ width: containerWidth(), height: props.height })
}

function onResize(): void {
  if (resizeTimer !== null) clearTimeout(resizeTimer)
  resizeTimer = setTimeout(() => {
    resizeTimer = null
    applySize()
  }, 100)
}

/** Repaint on a theme flip without touching the data. */
function applyColors(): void {
  if (plot === null) return
  const colors = readColors()
  plot.series.forEach((series, index) => {
    if (index === 0) return
    series.stroke = colors.series[(index - 1) % colors.series.length]
  })
  plot.axes.forEach((axis) => {
    axis.stroke = colors.axis
    if (axis.grid !== undefined) axis.grid.stroke = colors.grid
    if (axis.ticks !== undefined) axis.ticks.stroke = colors.grid
  })
  plot.redraw()
}

watch(
  data,
  (next) => {
    if (needsRebuild(next)) {
      create()
      return
    }
    plot?.setData(next)
    markRendered()
  },
  { flush: 'post' },
)

watch(
  () => props.height,
  () => applySize(),
)

onMounted(() => {
  create()
  observer = new ResizeObserver(onResize)
  if (host.value !== null) observer.observe(host.value)

  media = window.matchMedia('(prefers-color-scheme: dark)')
  media.addEventListener('change', applyColors)
  themeObserver = new MutationObserver(applyColors)
  themeObserver.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['class'],
  })
})

onBeforeUnmount(() => {
  plot?.destroy()
  plot = null
  observer?.disconnect()
  observer = null
  media?.removeEventListener('change', applyColors)
  media = null
  themeObserver?.disconnect()
  themeObserver = null
  if (resizeTimer !== null) clearTimeout(resizeTimer)
})
</script>

<template>
  <div
    ref="host"
    class="time-series-chart"
    data-testid="time-series-chart"
    :style="{ minHeight: `${height}px` }"
  />
</template>

<style scoped>
/* min-height, not height: uPlot draws its legend below the canvas, and a fixed
 * height clipped it inside the panel. */
.time-series-chart {
  position: relative;
  width: 100%;
}

/* uPlot lays its legend out with inline styles; these make it wrap legibly
 * inside a narrow panel without reaching for a colour of its own. */
.time-series-chart :deep(.u-legend) {
  font-size: var(--ds-font-size-xs);
}

.time-series-chart :deep(.u-legend .u-label) {
  color: var(--ds-text-muted);
}

.time-series-chart :deep(.u-legend .u-value) {
  color: var(--ds-text);
}
</style>
