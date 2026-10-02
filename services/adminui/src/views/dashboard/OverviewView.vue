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
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import { getOverview, type Overview, type ServiceOverview } from '@/api/dashboard'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { Breadcrumbs, ErrorPanel, Grid, PageHeader, StatTile, StatusDot } from '@/components/ui'
import type { StatusKind } from '@/components/ui'
import { usePolling } from '@/composables/usePolling'

/**
 * The overview: is anything down, and how loaded is the host.
 *
 * Read-only and deliberately shallow. The page polls rather than streaming,
 * because the numbers it shows move on the scale of a scrape interval and a
 * stream would be more machinery for the same picture. The poll is
 * visibility-aware (see `usePolling`): a background tab holds no timer and
 * sends nothing.
 *
 * The service cards are the dense half of the page. Ten of them have to fit
 * beside the host strip at 1440x900 without scrolling, so each is a fixed,
 * compact box rather than a panel. `degraded` is the service's own list of the
 * figures it could not compute; when it is non-empty the page says so at the top
 * and renders "—" in their place, never a zero.
 */

const overview = ref<Overview | null>(null)
const loading = ref(true)
const failure = ref<FailureMessage | null>(null)
const now = ref(Date.now())

const router = useRouter()

/** The relative "Updated 12 s ago" clock, ticked rather than recomputed on render. */
let clock: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  clock = setInterval(() => {
    now.value = Date.now()
  }, 1000)
})

onBeforeUnmount(() => {
  if (clock !== null) clearInterval(clock)
})

async function load(signal?: AbortSignal): Promise<void> {
  // Only the first load blanks the page with a spinner; a poll that fails keeps
  // the last picture and shows the error above it, and a poll that succeeds
  // does not flash the page every fifteen seconds.
  if (overview.value === null) loading.value = true
  failure.value = null
  try {
    overview.value = await getOverview(signal)
  } catch (error) {
    // A hide/unmount aborts the request; that is not a failure to report.
    if (signal?.aborted === true) return
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

function refresh(): void {
  void load()
}

// Auto-starts on mount and stops (aborting any in-flight request) on unmount.
usePolling({ fn: (signal) => load(signal) })

const down = computed(
  () => (overview.value?.services ?? []).filter((service) => !service.up).length,
)

/** Not up first, then not ready, then by name. */
function rank(service: ServiceOverview): number {
  if (!service.up) return 0
  if (!service.ready) return 1
  return 2
}

const services = computed(() =>
  [...(overview.value?.services ?? [])].sort(
    (a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name),
  ),
)

/**
 * The route UI-4 adds for a service's own page. Until it exists the card is not
 * a link; the check is by path rather than by a name that is not defined yet.
 */
const serviceDetailRoute = computed(
  () =>
    router.getRoutes().find((candidate) => candidate.path === '/dashboard/services/:name') ?? null,
)

function serviceTo(name: string): string | null {
  const route = serviceDetailRoute.value
  if (route === null || typeof route.name !== 'string') return null
  return router.resolve({ name: route.name, params: { name } }).path
}

const cards = computed(() =>
  services.value.map((service) => ({ service, to: serviceTo(service.name) })),
)

/** The three host ratios, in the order they are read. */
// The service names each card it could not compute ("host.disk",
// "services.p95_ms"); people read these, so they get words.
const DEGRADED_LABELS: Record<string, string> = {
  'host.cpu': 'CPU',
  'host.mem': 'memory',
  'host.memory': 'memory',
  'host.disk': 'disk',
  online_players: 'online players',
  alerts: 'alerts',
  'services.up': 'service status',
  'services.rps': 'request rates',
  'services.error_ratio': 'error rates',
  'services.p95_ms': 'latency',
  'services.version': 'versions',
}

const degradedText = computed(() =>
  (overview.value?.degraded ?? []).map((card) => DEGRADED_LABELS[card] ?? card).join(', '),
)

const meters = computed(() => {
  const host = overview.value?.host
  return [
    { label: 'CPU', value: host?.cpuRatio ?? null },
    { label: 'Memory', value: host?.memRatio ?? null },
    { label: 'Disk', value: host?.diskRatio ?? null },
  ]
})

function percent(value: number | null): string {
  return value === null ? '—' : `${Math.round(value * 100)}%`
}

function hostStatus(value: number | null): StatusKind | undefined {
  if (value === null) return undefined
  if (value >= 0.95) return 'danger'
  if (value >= 0.8) return 'warn'
  return 'ok'
}

function statusOf(service: ServiceOverview): StatusKind {
  if (!service.up) return 'danger'
  if (!service.ready) return 'warn'
  return 'ok'
}

function labelOf(service: ServiceOverview): string {
  if (!service.up) return 'down'
  if (!service.ready) return 'not ready'
  return 'ready'
}

function reasonFallback(service: ServiceOverview): string {
  return service.up ? 'not ready' : 'not up'
}

const VERSION_LIMIT = 12

function shortVersion(version: string): string {
  return version.length > VERSION_LIMIT ? version.slice(0, VERSION_LIMIT) : version
}

function rps(value: number | null): string {
  return value === null ? '—' : value.toFixed(1)
}

function ratio(value: number | null): string {
  return value === null ? '—' : `${(value * 100).toFixed(1)}%`
}

function ms(value: number | null): string {
  return value === null ? '—' : String(Math.round(value))
}

function relativeTime(from: string, current: number): string {
  const at = Date.parse(from)
  if (Number.isNaN(at)) return 'unknown'
  const seconds = Math.max(0, Math.round((current - at) / 1000))
  if (seconds < 5) return 'just now'
  if (seconds < 60) return `${seconds} s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} min ago`
  return `${Math.floor(minutes / 60)} h ago`
}

const updated = computed(() => {
  const at = overview.value?.generatedAt
  return at === undefined ? 'unknown' : relativeTime(at, now.value)
})

const players = computed(() => overview.value?.onlinePlayers ?? null)

const alerts = computed(() => overview.value?.alerts ?? [])

/** The panel is an assertive alert when anything in it is critical. */
const alertsCritical = computed(() => alerts.value.some((alert) => alert.severity === 'critical'))

/** A label the page knows how to paint; anything else is "other". */
function alertSeverity(severity: string): 'critical' | 'warning' | 'other' {
  if (severity === 'critical') return 'critical'
  if (severity === 'warning') return 'warning'
  return 'other'
}

function alertSummary(alert: { name: string; summary: string }): string {
  return alert.summary || alert.name
}
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
      <div class="ds-page">
        <PageHeader title="Overview">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <IonButton fill="clear" :disabled="loading" @click="refresh">Refresh</IonButton>
          </template>
        </PageHeader>

        <div v-if="loading" class="overview__busy">
          <IonSpinner name="dots" />
          <p>Loading the overview...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="refresh"
        />

        <template v-else-if="overview !== null">
          <p class="overview__meta">
            Updated {{ updated }}
            <span v-if="down > 0" class="overview__down">
              &middot; {{ down }} {{ down === 1 ? 'service is' : 'services are' }} down
            </span>
          </p>

          <!-- The page says what is missing rather than blanking; the figures
               themselves render as an em dash below. -->
          <p v-if="overview.degraded.length > 0" class="overview__degraded" role="status">
            Some figures are unavailable right now: {{ degradedText }}. The rest of the page is
            current.
          </p>

          <!-- Firing alerts sit above everything: the page's job is "is anything
               wrong", and this is the service's own answer to that. -->
          <section
            v-if="alerts.length > 0"
            class="overview__alerts"
            :role="alertsCritical ? 'alert' : 'status'"
            data-testid="overview-alerts"
          >
            <h2 class="overview__alerts-heading">Firing alerts ({{ alerts.length }})</h2>
            <ul class="overview__alerts-list">
              <li
                v-for="(alert, index) in alerts"
                :key="`${alert.name}-${alert.activeAt}-${index}`"
                class="overview__alert"
                data-testid="overview-alert"
              >
                <span
                  class="overview__alert-badge"
                  :class="`overview__alert-badge--${alertSeverity(alert.severity)}`"
                >
                  {{ alertSeverity(alert.severity) }}
                </span>
                <span class="overview__alert-summary">{{ alertSummary(alert) }}</span>
                <span v-if="alert.service !== ''" class="overview__alert-service">
                  {{ alert.service }}
                </span>
                <span class="overview__alert-time">
                  since {{ relativeTime(alert.activeAt, now) }}
                </span>
              </li>
            </ul>
          </section>

          <Grid class="overview__tiles" :min="200" :gap="4">
            <StatTile
              v-for="meter in meters"
              :key="meter.label"
              :label="meter.label"
              :value="percent(meter.value)"
              :status="hostStatus(meter.value)"
              :hint="meter.value === null ? 'unavailable' : undefined"
            />
            <StatTile
              label="Online players"
              :value="players === null ? '—' : players"
              unit="players"
              :hint="players === null ? 'unavailable' : undefined"
            />
          </Grid>

          <Grid class="overview__services" :min="260" :gap="3">
            <component
              :is="card.to === null ? 'article' : RouterLink"
              v-for="card in cards"
              :key="card.service.name"
              v-bind="card.to === null ? {} : { to: card.to }"
              class="overview__card"
              data-testid="service-card"
            >
              <div class="overview__card-head">
                <StatusDot
                  :status="statusOf(card.service)"
                  :label="labelOf(card.service)"
                  compact
                />
                <span class="overview__card-name" :title="card.service.name">
                  {{ card.service.name }}
                </span>
                <code
                  v-if="card.service.version !== ''"
                  class="overview__card-version"
                  :title="card.service.version"
                >
                  {{ shortVersion(card.service.version) }}
                </code>
              </div>

              <p
                v-if="!card.service.up || !card.service.ready"
                class="overview__reason"
                :title="card.service.reason"
              >
                {{ card.service.reason || reasonFallback(card.service) }}
              </p>

              <p class="overview__figures">
                <span>rps {{ rps(card.service.rps) }}</span>
                <span>err {{ ratio(card.service.errorRatio) }}</span>
                <span>p95 {{ ms(card.service.p95Ms) }} ms</span>
              </p>
            </component>
          </Grid>
        </template>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.overview__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.overview__meta {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.overview__down {
  color: var(--ds-danger);
}

.overview__degraded {
  background: var(--ds-warn-soft);
  border: 1px solid var(--ds-warn);
  border-radius: var(--ds-radius-md);
  color: var(--ds-text);
  margin: 0 0 var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.overview__alerts {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  box-shadow: var(--ds-shadow-1);
  margin: 0 0 var(--ds-space-4);
  padding: var(--ds-space-3);
}

.overview__alerts-heading {
  font-size: var(--ds-font-size-md);
  margin: 0 0 var(--ds-space-2);
}

.overview__alerts-list {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  list-style: none;
  margin: 0;
  padding: 0;
}

.overview__alert {
  align-items: baseline;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  font-size: var(--ds-font-size-sm);
}

.overview__alert-badge {
  border-radius: var(--ds-radius-sm);
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  padding: 0 var(--ds-space-2);
  text-transform: uppercase;
}

.overview__alert-badge--critical {
  background: var(--ds-danger-soft);
  color: var(--ds-danger);
}

.overview__alert-badge--warning {
  background: var(--ds-warn-soft);
  color: var(--ds-warn);
}

.overview__alert-badge--other {
  background: var(--ds-neutral-soft);
  color: var(--ds-neutral);
}

.overview__alert-summary {
  font-weight: 600;
}

.overview__alert-service,
.overview__alert-time {
  color: var(--ds-text-muted);
}

.overview__alert-time {
  margin-left: auto;
}

.overview__tiles {
  margin-bottom: var(--ds-space-4);
}

/* Fixed and compact so ten cards sit on one 1440x900 screen below the tiles. */
.overview__card {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  box-shadow: var(--ds-shadow-1);
  color: var(--ds-text);
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
  height: 6.5rem;
  overflow: hidden;
  padding: var(--ds-space-3);
  text-decoration: none;
}

.overview__card-head {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
  min-width: 0;
}

.overview__card-name {
  flex: 1;
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.overview__card-version {
  color: var(--ds-text-muted);
  flex: none;
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
}

.overview__reason {
  color: var(--ds-warn);
  font-size: var(--ds-font-size-xs);
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.overview__figures {
  color: var(--ds-text-muted);
  display: flex;
  font-size: var(--ds-font-size-xs);
  gap: var(--ds-space-3);
  margin: 0;
  overflow: hidden;
  white-space: nowrap;
}
</style>
