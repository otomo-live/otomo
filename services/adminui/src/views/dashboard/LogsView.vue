<script setup lang="ts">
import { IonContent, IonHeader, IonMenuButton, IonPage, IonSpinner, IonToolbar } from '@ionic/vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { listLogs, listServices, type LogLine } from '@/api/dashboard'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { isTooManyTails, tailLogs, type LogTail } from '@/api/sse'
import LogList from '@/components/logs/LogList.vue'
import {
  Breadcrumbs,
  EmptyState,
  ErrorPanel,
  PageHeader,
  StatusDot,
  Toolbar,
} from '@/components/ui'
import type { StatusKind } from '@/components/ui'
import {
  CUSTOM_RANGE_KEY,
  LOG_LEVELS,
  LOG_RANGES,
  logFiltersQuery,
  parseLogFilters,
  requestIdOf,
  resolveLogWindow,
} from '@/logs/filters'
import { RingBuffer } from '@/logs/ringBuffer'

/**
 * The log explorer and live tail.
 *
 * History and stream, one list. `listLogs` fills the view the moment it opens so
 * a quiet system is not a blank box, and "load more" walks backwards through
 * older pages up to ten thousand lines. The live tail then prepends what happens
 * next, newest at the top.
 *
 * The tail buffer is a fixed 5000-line ring: a page left open overnight would
 * otherwise hold an unbounded list, and the browser would fail before the
 * service did. Arriving lines are batched and spliced once per animation frame,
 * so a fast tail costs one buffer write per frame rather than one per line.
 *
 * Every filter is URL state, so the link is the query. That also makes the view
 * stateless across reloads: parse the query, fetch, render.
 */

const MAX_TAIL_LINES = 5000
const MAX_HISTORY_LINES = 10_000
const PAGE_SIZE = 500
const CONTAINS_DEBOUNCE_MS = 300

const route = useRoute()
const router = useRouter()

const filters = computed(() => parseLogFilters(route.query as Record<string, unknown>))

const services = ref<string[]>([])
const history = ref<LogLine[]>([])
const tailLines = ref<LogLine[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const failure = ref<FailureMessage | null>(null)
const tailFailure = ref<FailureMessage | null>(null)
const canLoadMore = ref(false)

const live = ref(false)
const paused = ref(false)
const reconnects = ref(0)
const unseen = ref(0)
const atTop = ref(true)

/** The text field's own state: applied to the URL only after the debounce. */
const containsInput = ref(filters.value.contains)

const logList = ref<InstanceType<typeof LogList> | null>(null)
let tail: LogTail | null = null
let containsTimer: ReturnType<typeof setTimeout> | null = null

const tailRing = new RingBuffer<LogLine>(MAX_TAIL_LINES)
let pendingTail: LogLine[] = []
let frameHandle: number | null = null

const rows = computed(() => [...tailLines.value, ...history.value])

const status = computed(() => {
  if (tailFailure.value !== null) return tailFailure.value.summary
  if (!live.value) return 'Not streaming.'
  if (paused.value) return 'Paused while the tab is hidden.'
  if (reconnects.value > 0) return `Reconnecting (attempt ${reconnects.value}).`
  return 'Streaming.'
})

const statusKind = computed<StatusKind>(() => {
  if (tailFailure.value !== null) return 'danger'
  if (!live.value || paused.value) return 'neutral'
  if (reconnects.value > 0) return 'warn'
  return 'ok'
})

const showJumpPill = computed(() => !atTop.value && unseen.value > 0)

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

function lineIdentity(line: LogLine): string {
  return `${line.at}|${line.service}|${line.level}|${line.message}`
}

function queryFor(to?: string) {
  const window = resolveLogWindow(filters.value, Date.now())
  return {
    service: filters.value.service,
    level: filters.value.level,
    contains: filters.value.contains,
    requestId: filters.value.requestId,
    from: window.from,
    // "Load more" walks back from the oldest line already held.
    to: to ?? window.to,
    limit: PAGE_SIZE,
  }
}

async function loadHistory(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const page = await listLogs(queryFor())
    history.value = page
    canLoadMore.value = page.length === PAGE_SIZE && page.length < MAX_HISTORY_LINES
  } catch (error) {
    failure.value = describeFailure(error)
    history.value = []
    canLoadMore.value = false
  } finally {
    loading.value = false
  }
}

async function loadMore(): Promise<void> {
  if (!canLoadMore.value || loadingMore.value) return
  const oldest = history.value[history.value.length - 1]
  if (oldest === undefined) return

  loadingMore.value = true
  try {
    // `to` is inclusive and asked for at the oldest line's own timestamp: logs come
    // in bursts, and stepping past it (even by a millisecond) would skip every
    // line that shares that instant but did not fit on the previous page. The
    // lines already held come back too and are dropped by identity below.
    const page = await listLogs(queryFor(oldest.at))
    const seen = new Set(history.value.map(lineIdentity))
    const fresh = page.filter((line) => !seen.has(lineIdentity(line)))
    const combined = [...history.value, ...fresh]
    history.value =
      combined.length > MAX_HISTORY_LINES ? combined.slice(0, MAX_HISTORY_LINES) : combined
    // A full page that added nothing new is a burst larger than a page at one
    // instant; stop rather than ask for the same page forever.
    canLoadMore.value =
      page.length === PAGE_SIZE && fresh.length > 0 && history.value.length < MAX_HISTORY_LINES
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loadingMore.value = false
  }
}

// ---------------------------------------------------------------------------
// Live tail
// ---------------------------------------------------------------------------

function matchesTailFilters(line: LogLine): boolean {
  const wanted = filters.value.requestId
  if (wanted === '') return true
  // The tail endpoint has no request-id parameter, so the one filter it cannot
  // apply on the server is applied here, on the way in.
  return requestIdOf(line) === wanted
}

function flushTail(): void {
  frameHandle = null
  if (pendingTail.length === 0) return
  const added = pendingTail.length
  tailRing.prependAll(pendingTail)
  pendingTail = []
  tailLines.value = tailRing.toArray()

  if (atTop.value) {
    void nextTick(() => logList.value?.scrollToTop())
  } else {
    unseen.value += added
  }
}

function onTailLine(line: LogLine): void {
  if (!matchesTailFilters(line)) return
  pendingTail.push(line)
  if (frameHandle === null) frameHandle = requestAnimationFrame(flushTail)
}

function onTailError(error: unknown): void {
  if (isTooManyTails(error)) {
    live.value = false
    paused.value = false
    tailFailure.value = {
      summary: 'Too many live tails open (limit 2 per person). Close one in another tab.',
      detail: describeFailure(error).detail,
      reference: describeFailure(error).reference,
    }
    return
  }
  tailFailure.value = describeFailure(error)
}

function startTail(): void {
  if (tail !== null || !live.value || typeof document === 'undefined') return
  if (document.visibilityState === 'hidden') {
    paused.value = true
    return
  }
  reconnects.value = 0
  tailFailure.value = null
  tail = tailLogs({
    service: filters.value.service,
    level: filters.value.level,
    contains: filters.value.contains,
    onLine: onTailLine,
    onError: onTailError,
    onReconnect: (attempt) => {
      reconnects.value = attempt
    },
  })
}

function stopTail(): void {
  tail?.stop()
  tail = null
  if (frameHandle !== null) {
    cancelAnimationFrame(frameHandle)
    frameHandle = null
  }
  pendingTail = []
}

function stopLive(): void {
  live.value = false
  paused.value = false
  stopTail()
}

function toggleLive(): void {
  if (live.value) {
    stopLive()
    return
  }
  live.value = true
  tailFailure.value = null
  unseen.value = 0
  startTail()
}

function jumpToLatest(): void {
  unseen.value = 0
  atTop.value = true
  logList.value?.scrollToTop()
}

// ---------------------------------------------------------------------------
// Filters, all in the URL
// ---------------------------------------------------------------------------

function updateFilters(patch: Partial<ReturnType<typeof parseLogFilters>>): void {
  void router.replace({ query: logFiltersQuery({ ...filters.value, ...patch }) })
}

function onServiceInput(event: Event): void {
  updateFilters({ service: (event.target as HTMLInputElement).value.trim() })
}

function onLevelInput(event: Event): void {
  updateFilters({ level: (event.target as HTMLSelectElement).value })
}

function onContainsInput(event: Event): void {
  const value = (event.target as HTMLInputElement).value
  containsInput.value = value
  if (containsTimer !== null) clearTimeout(containsTimer)
  containsTimer = setTimeout(() => {
    containsTimer = null
    updateFilters({ contains: value })
  }, CONTAINS_DEBOUNCE_MS)
}

function onRequestIdInput(event: Event): void {
  updateFilters({ requestId: (event.target as HTMLInputElement).value.trim() })
}

function onRangeInput(event: Event): void {
  const range = (event.target as HTMLSelectElement).value
  // A named range resolves against "now", so any previously typed custom bounds
  // must go with it or they would keep filtering a range that no longer has them.
  updateFilters({ range, from: '', to: '' })
}

function onFromInput(event: Event): void {
  updateFilters({ from: (event.target as HTMLInputElement).value })
}

function onToInput(event: Event): void {
  updateFilters({ to: (event.target as HTMLInputElement).value })
}

function onRequestIdFollow(id: string): void {
  updateFilters({ requestId: id })
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

async function reload(): Promise<void> {
  stopTail()
  tailRing.clear()
  tailLines.value = []
  unseen.value = 0
  atTop.value = true
  history.value = []
  canLoadMore.value = false
  await loadHistory()
  if (live.value) startTail()
}

watch(
  () => route.query,
  () => {
    void reload()
  },
  { deep: true },
)

watch(
  () => filters.value.contains,
  (value) => {
    containsInput.value = value
  },
)

function onVisibilityChange(): void {
  if (document.visibilityState === 'hidden') {
    if (live.value && tail !== null) {
      stopTail()
      paused.value = true
    }
    return
  }
  if (live.value && paused.value) {
    paused.value = false
    startTail()
  }
}

onMounted(() => {
  document.addEventListener('visibilitychange', onVisibilityChange)
  void (async () => {
    try {
      services.value = (await listServices()).map((entry) => entry.name)
    } catch {
      // A missing service list only costs the filter its suggestions.
      services.value = []
    }
  })()
  void reload()
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibilityChange)
  if (containsTimer !== null) clearTimeout(containsTimer)
  stopLive()
})
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <!-- Opens the shell's navigation below the `when="md"`
             breakpoint, where the menu is a drawer. -->
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="logs-page" :data-line-count="rows.length">
        <PageHeader title="Logs">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <button
              type="button"
              class="logs__toggle"
              data-testid="tail-toggle"
              @click="toggleLive"
            >
              {{ live ? 'Stop' : 'Start' }} live tail
            </button>
          </template>
        </PageHeader>

        <Toolbar class="logs__filters">
          <label class="logs__field">
            <span>Service</span>
            <input
              :value="filters.service"
              list="logs-services"
              placeholder="Every service"
              data-testid="filter-service"
              @change="onServiceInput"
            />
          </label>
          <datalist id="logs-services">
            <option v-for="name in services" :key="name" :value="name" />
          </datalist>

          <label class="logs__field">
            <span>Level</span>
            <select :value="filters.level" data-testid="filter-level" @change="onLevelInput">
              <option value="">Every level</option>
              <option v-for="name in LOG_LEVELS" :key="name" :value="name">{{ name }}</option>
            </select>
          </label>

          <label class="logs__field">
            <span>Contains</span>
            <input
              :value="containsInput"
              placeholder="substring"
              data-testid="filter-contains"
              @input="onContainsInput"
            />
          </label>

          <label class="logs__field">
            <span>Request id</span>
            <input
              :value="filters.requestId"
              placeholder="req_…"
              data-testid="filter-request-id"
              @change="onRequestIdInput"
            />
          </label>

          <label class="logs__field">
            <span>Range</span>
            <select :value="filters.range" data-testid="filter-range" @change="onRangeInput">
              <option v-for="range in LOG_RANGES" :key="range.key" :value="range.key">
                {{ range.label }}
              </option>
              <option :value="CUSTOM_RANGE_KEY">Custom range</option>
            </select>
          </label>

          <template v-if="filters.range === CUSTOM_RANGE_KEY">
            <label class="logs__field">
              <span>From</span>
              <input
                type="datetime-local"
                :value="filters.from"
                data-testid="filter-from"
                @change="onFromInput"
              />
            </label>
            <label class="logs__field">
              <span>To</span>
              <input
                type="datetime-local"
                :value="filters.to"
                data-testid="filter-to"
                @change="onToInput"
              />
            </label>
          </template>
        </Toolbar>

        <p class="logs__status" data-testid="tail-status">
          <IonSpinner v-if="live && reconnects > 0 && !paused" name="dots" />
          <StatusDot :status="statusKind" :label="status" />
          <span v-if="MAX_TAIL_LINES === tailRing.length">
            &middot; the live buffer holds its most recent {{ MAX_TAIL_LINES }} lines</span
          >
        </p>

        <ErrorPanel
          v-if="tailFailure !== null"
          data-testid="tail-error"
          :message="tailFailure.summary"
          :detail="tailFailure.detail"
          :request-id="tailFailure.reference"
        />

        <ErrorPanel
          v-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="reload"
        />

        <EmptyState
          v-else-if="rows.length === 0 && !loading"
          title="Nothing to show"
          detail="No lines match these filters, and nothing has arrived on the stream yet."
        />

        <LogList
          v-else
          ref="logList"
          :rows="rows"
          :loading="loading"
          @request-id="onRequestIdFollow"
          @at-top="atTop = $event"
        />

        <div class="logs__footer">
          <button
            v-if="canLoadMore"
            type="button"
            class="logs__more"
            data-testid="load-more"
            :disabled="loadingMore"
            @click="loadMore"
          >
            <IonSpinner v-if="loadingMore" name="dots" />
            <span v-else>Load more (older)</span>
          </button>
          <span class="logs__count" data-testid="log-count">{{ rows.length }} lines</span>
        </div>

        <button
          v-if="showJumpPill"
          type="button"
          class="logs__pill"
          data-testid="log-new-pill"
          @click="jumpToLatest"
        >
          {{ unseen }} new line{{ unseen === 1 ? '' : 's' }} — jump to latest
        </button>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.logs__filters {
  margin-bottom: var(--ds-space-4);
}

.logs__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.logs__field > span {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.logs__field input,
.logs__field select {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.logs__toggle,
.logs__more {
  background: var(--ds-accent);
  border: 1px solid transparent;
  border-radius: var(--ds-radius-sm);
  color: var(--ds-accent-contrast);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2) var(--ds-space-4);
}

.logs__more[disabled] {
  opacity: 0.6;
}

.logs__status {
  align-items: center;
  color: var(--ds-text-muted);
  display: flex;
  font-size: var(--ds-font-size-sm);
  gap: var(--ds-space-2);
  margin: 0 0 var(--ds-space-3);
}

.logs__footer {
  align-items: center;
  display: flex;
  gap: var(--ds-space-3);
  justify-content: space-between;
  margin-top: var(--ds-space-3);
}

.logs__count {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.logs__pill {
  background: var(--ds-accent);
  border: none;
  border-radius: 999px;
  bottom: var(--ds-space-5);
  color: var(--ds-accent-contrast);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  left: 50%;
  padding: var(--ds-space-2) var(--ds-space-4);
  position: fixed;
  transform: translateX(-50%);
  z-index: 2;
}
</style>
