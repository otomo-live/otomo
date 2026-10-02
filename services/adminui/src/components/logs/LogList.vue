<script setup lang="ts">
import { useVirtualizer } from '@tanstack/vue-virtual'
import { computed, nextTick, ref, shallowRef, type ComponentPublicInstance } from 'vue'

import type { LogLine } from '@/api/dashboard'
import { requestIdOf } from '@/logs/filters'

/**
 * The virtualised log table.
 *
 * A browser asked to lay out ten thousand sibling rows will do it, and then
 * stall. Only the rows inside (plus a little either side of) the viewport are
 * rendered; the rest are replaced by one spacer of the measured total height, so
 * the scrollbar still describes the whole list. Row heights are measured rather
 * than assumed, because a row expands to show its fields and an estimated height
 * would leave a gap under every open line.
 *
 * Newest is at the top, which is the order the endpoint returns and the order a
 * tail reads in.
 */

const props = defineProps<{
  rows: LogLine[]
  loading?: boolean
}>()

const emit = defineEmits<{ 'request-id': [id: string]; 'at-top': [atTop: boolean] }>()

const ROW_ESTIMATE_PX = 44

const scrollRef = ref<HTMLElement | null>(null)
/** Keyed by the line object itself, so a prepended batch does not disturb it. */
const expanded = shallowRef<Set<LogLine>>(new Set())

const virtualizer = useVirtualizer(
  computed(() => ({
    count: props.rows.length,
    getScrollElement: () => scrollRef.value,
    estimateSize: () => ROW_ESTIMATE_PX,
    overscan: 12,
    getItemKey: (index: number) => keyOf(props.rows[index]),
  })),
)

const virtualRows = computed(() => virtualizer.value.getVirtualItems())
const totalSize = computed(() => virtualizer.value.getTotalSize())

/**
 * A stable numeric identity per line object. The virtualiser caches measurements
 * by this key, so it has to stay put while the list is reordered underneath it.
 */
const keyIds = new WeakMap<LogLine, number>()
let nextKey = 1

function keyOf(line: LogLine | undefined): number {
  if (line === undefined) return -1
  const existing = keyIds.get(line)
  if (existing !== undefined) return existing
  const key = nextKey
  nextKey += 1
  keyIds.set(line, key)
  return key
}

function measureRef(element: Element | ComponentPublicInstance | null): void {
  virtualizer.value.measureElement(element instanceof Element ? element : null)
}

function isExpanded(line: LogLine): boolean {
  return expanded.value.has(line)
}

function toggle(line: LogLine): void {
  const next = new Set(expanded.value)
  if (next.has(line)) next.delete(line)
  else next.add(line)
  expanded.value = next
  // The open row is taller now; measure it so the rows below move down.
  void nextTick(() => virtualizer.value.measure())
}

function prettyFields(line: LogLine): string {
  if (line.fields === null) return ''
  try {
    return JSON.stringify(line.fields, null, 2)
  } catch {
    // A field that cannot round-trip (a bigint, a cycle) still has to render.
    return String(line.fields)
  }
}

const copied = ref<LogLine | null>(null)
let copyTimer: ReturnType<typeof setTimeout> | null = null

async function copy(line: LogLine): Promise<void> {
  try {
    const clipboard = navigator.clipboard
    if (clipboard === undefined || typeof clipboard.writeText !== 'function') return
    await clipboard.writeText(prettyFields(line))
  } catch {
    return
  }
  copied.value = line
  if (copyTimer !== null) clearTimeout(copyTimer)
  copyTimer = setTimeout(() => {
    copied.value = null
    copyTimer = null
  }, 1500)
}

/** Local time to the millisecond: the tail is read at that resolution. */
function formatTime(value: string): string {
  const milliseconds = Date.parse(value)
  if (Number.isNaN(milliseconds)) return value
  const at = new Date(milliseconds)
  const base = at.toLocaleString(undefined, { hour12: false })
  return `${base}.${String(at.getMilliseconds()).padStart(3, '0')}`
}

function levelKind(level: string): string {
  switch (level) {
    case 'debug':
      return 'neutral'
    case 'info':
      return 'info'
    case 'warn':
      return 'warn'
    case 'error':
      return 'danger'
    default:
      return 'neutral'
  }
}

/** Within this many pixels of the top counts as "watching the newest". */
const TOP_THRESHOLD_PX = 8
let reportedAtTop = true

function onScroll(): void {
  const element = scrollRef.value
  if (element === null) return
  const nowAtTop = element.scrollTop <= TOP_THRESHOLD_PX
  if (nowAtTop !== reportedAtTop) {
    reportedAtTop = nowAtTop
    emit('at-top', nowAtTop)
  }
}

/** Pin the newest line to the top; the view calls this on a prepend. */
function scrollToTop(): void {
  if (scrollRef.value !== null) scrollRef.value.scrollTop = 0
}

defineExpose({ scrollToTop })
</script>

<template>
  <div ref="scrollRef" class="logs__scroll" data-testid="log-scroll" @scroll.passive="onScroll">
    <div class="logs__canvas" :style="{ height: `${totalSize}px` }">
      <div
        v-for="row in virtualRows"
        :key="keyOf(props.rows[row.index])"
        :ref="measureRef"
        class="logs__row"
        data-testid="log-row"
        :data-index="row.index"
        :style="{ transform: `translateY(${row.start}px)` }"
      >
        <div
          class="logs__summary"
          role="button"
          tabindex="0"
          :aria-expanded="isExpanded(props.rows[row.index])"
          @click="toggle(props.rows[row.index])"
          @keydown.enter.prevent="toggle(props.rows[row.index])"
          @keydown.space.prevent="toggle(props.rows[row.index])"
        >
          <span class="logs__time">{{ formatTime(props.rows[row.index].at) }}</span>
          <span
            class="logs__level"
            :class="`logs__level--${levelKind(props.rows[row.index].level)}`"
          >
            {{ props.rows[row.index].level }}
          </span>
          <span class="logs__service">{{ props.rows[row.index].service }}</span>
          <span class="logs__message">{{ props.rows[row.index].message }}</span>
        </div>

        <div v-if="isExpanded(props.rows[row.index])" class="logs__detail" data-testid="log-detail">
          <p class="logs__full">{{ props.rows[row.index].message }}</p>
          <template v-if="props.rows[row.index].fields !== null">
            <div class="logs__fields">
              <pre class="logs__json" data-testid="log-fields">{{
                prettyFields(props.rows[row.index])
              }}</pre>
              <button type="button" class="logs__copy" @click.stop="copy(props.rows[row.index])">
                {{ copied === props.rows[row.index] ? 'Copied' : 'Copy' }}
              </button>
            </div>
            <p v-if="requestIdOf(props.rows[row.index]) !== null" class="logs__request">
              request_id
              <button
                type="button"
                class="logs__link"
                data-testid="log-request-link"
                @click.stop="emit('request-id', requestIdOf(props.rows[row.index]) as string)"
              >
                {{ requestIdOf(props.rows[row.index]) }}
              </button>
            </p>
          </template>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.logs__scroll {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  height: 70vh;
  min-height: 320px;
  overflow: auto;
  position: relative;
}

.logs__canvas {
  position: relative;
  width: 100%;
}

.logs__row {
  left: 0;
  position: absolute;
  top: 0;
  width: 100%;
}

.logs__summary {
  align-items: center;
  border-bottom: 1px solid var(--ds-border);
  cursor: pointer;
  display: grid;
  font-size: var(--ds-font-size-sm);
  gap: var(--ds-space-3);
  grid-template-columns: 13rem 4.5rem 8rem minmax(0, 1fr);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.logs__summary:hover {
  background: var(--ds-neutral-soft);
}

.logs__summary:focus-visible {
  outline: 2px solid var(--ds-accent);
  outline-offset: -2px;
}

.logs__time,
.logs__service {
  color: var(--ds-text-muted);
  font-family: var(--ds-font-mono);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.logs__level {
  border-radius: var(--ds-radius-sm);
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  justify-self: start;
  letter-spacing: 0.04em;
  padding: 1px var(--ds-space-1);
  text-transform: uppercase;
}

.logs__level--neutral {
  background: var(--ds-neutral-soft);
  color: var(--ds-neutral);
}

.logs__level--info {
  background: var(--ds-info-soft);
  color: var(--ds-info);
}

.logs__level--warn {
  background: var(--ds-warn-soft);
  color: var(--ds-warn);
}

.logs__level--danger {
  background: var(--ds-danger-soft);
  color: var(--ds-danger);
}

.logs__message {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.logs__detail {
  background: var(--ds-surface-raised);
  border-bottom: 1px solid var(--ds-border);
  padding: var(--ds-space-3);
}

.logs__full {
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-2);
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}

.logs__fields {
  position: relative;
}

.logs__json {
  background: var(--ds-bg);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
  margin: 0;
  max-height: 16rem;
  overflow: auto;
  padding: var(--ds-space-2);
  white-space: pre;
}

.logs__copy {
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
  position: absolute;
  right: var(--ds-space-2);
  top: var(--ds-space-2);
}

.logs__copy:hover {
  color: var(--ds-text);
}

.logs__request {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  margin: var(--ds-space-2) 0 0;
}

.logs__link {
  background: none;
  border: none;
  color: var(--ds-accent);
  cursor: pointer;
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
  padding: 0;
  text-decoration: underline;
}
</style>
