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
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { listMergedAudit, type MergedAuditEntry } from '@/api/dashboard'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { isRecord } from '@/api/shape'
import { AUDIT_SOURCES, auditFiltersQuery, parseAuditFilters } from '@/audit/filters'
import {
  Breadcrumbs,
  DataTable,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Toolbar,
} from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'

/**
 * The merged audit: who changed what, across the services that keep a log of it.
 *
 * The `source` badge is not decoration. Config's own audit table has no such
 * column (design/02-config.md section 3); the field exists because Dashboard fans
 * out to more than one service and has to say which one a row came from
 * (DSH-C11). A row with no provenance in a merged list is a row a reader cannot
 * chase.
 *
 * Filters are URL state, and a page failure is not the same as an empty one: a
 * source Dashboard could not read is named in a notice and the rows that did
 * arrive are still shown, because a trail that is quietly missing half of itself
 * is worse than one that says so.
 *
 * A `release.publish` (or rollback) row carries the release ids in its details,
 * so it links to the diff between that release and its base: the audit says what
 * happened and the diff says what changed.
 */

const route = useRoute()
const router = useRouter()

const filters = computed(() => parseAuditFilters(route.query as Record<string, unknown>))

const entries = ref<MergedAuditEntry[]>([])
const cursor = ref<string | null>(null)
const loading = ref(true)
const loadingMore = ref(false)
const failure = ref<FailureMessage | null>(null)
const degraded = ref<string[]>([])
/** Expanded details, keyed by `source:id`, because a raw id repeats across sources. */
const expanded = ref<string[]>([])

const columns: DataTableColumn[] = [
  { key: 'at', label: 'Time' },
  { key: 'source', label: 'Source' },
  { key: 'actor', label: 'Actor' },
  { key: 'action', label: 'Action', mono: true },
  { key: 'target', label: 'Target', mono: true },
  { key: 'details', label: 'Details', width: '28%' },
]

/** The actions whose details carry both a release id and the base it was built on. */
const RELEASE_ACTIONS = new Set(['release.publish', 'release.rollback', 'release.promote'])

function queryFor(cursor?: string) {
  return {
    source: filters.value.source,
    actor: filters.value.actor,
    from: filters.value.from,
    to: filters.value.to,
    cursor,
  }
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const page = await listMergedAudit(queryFor())
    entries.value = page.entries
    cursor.value = page.nextCursor
    degraded.value = page.degraded
  } catch (error) {
    failure.value = describeFailure(error)
    entries.value = []
    cursor.value = null
    degraded.value = []
  } finally {
    loading.value = false
  }
}

async function loadMore(): Promise<void> {
  if (cursor.value === null || loadingMore.value) return
  loadingMore.value = true
  failure.value = null
  try {
    const page = await listMergedAudit(queryFor(cursor.value))
    entries.value = [...entries.value, ...page.entries]
    cursor.value = page.nextCursor
    degraded.value = page.degraded
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loadingMore.value = false
  }
}

function updateFilters(patch: Partial<ReturnType<typeof parseAuditFilters>>): void {
  void router.replace({ query: auditFiltersQuery({ ...filters.value, ...patch }) })
}

function onSourceInput(event: Event): void {
  updateFilters({ source: (event.target as HTMLSelectElement).value })
}

function onActorInput(event: Event): void {
  updateFilters({ actor: (event.target as HTMLInputElement).value.trim() })
}

function onFromInput(event: Event): void {
  updateFilters({ from: (event.target as HTMLInputElement).value })
}

function onToInput(event: Event): void {
  updateFilters({ to: (event.target as HTMLInputElement).value })
}

function timestamp(value: string): string {
  const at = Date.parse(value)
  return Number.isNaN(at) ? value : new Date(at).toLocaleString()
}

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

/** The local time the cell shows, with the distance from now on hover. */
function relativeTime(value: string): string {
  const at = Date.parse(value)
  if (Number.isNaN(at)) return value
  const seconds = (at - Date.now()) / 1000
  const abs = Math.abs(seconds)
  if (abs < 45) return relative.format(Math.round(seconds), 'second')
  if (abs < 3600) return relative.format(Math.round(seconds / 60), 'minute')
  if (abs < 86400) return relative.format(Math.round(seconds / 3600), 'hour')
  if (abs < 2592000) return relative.format(Math.round(seconds / 86400), 'day')
  if (abs < 31536000) return relative.format(Math.round(seconds / 2592000), 'month')
  return relative.format(Math.round(seconds / 31536000), 'year')
}

function hasDetails(entry: MergedAuditEntry): boolean {
  const value = entry.details
  if (value === null || value === undefined) return false
  if (Array.isArray(value)) return value.length > 0
  if (typeof value === 'object') return Object.keys(value).length > 0
  return true
}

function prettyDetails(entry: MergedAuditEntry): string {
  if (entry.details === null || entry.details === undefined) return ''
  return JSON.stringify(entry.details, null, 2) ?? String(entry.details)
}

function entryKey(entry: MergedAuditEntry): string {
  return `${entry.source}:${entry.id}`
}

function isExpanded(entry: MergedAuditEntry): boolean {
  return expanded.value.includes(entryKey(entry))
}

function toggleDetails(entry: MergedAuditEntry): void {
  const key = entryKey(entry)
  expanded.value = isExpanded(entry)
    ? expanded.value.filter((candidate) => candidate !== key)
    : [...expanded.value, key]
}

function positiveId(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : null
}

/**
 * Where a release row's "View changes" goes, or null for a row that has nothing
 * to diff. Publish names the release and its base; a rollback names the release
 * it moved to and the one it moved from. A promote carries its release id but no
 * base (its content came from another channel), so it has no diff on this one.
 */
function changesPath(entry: MergedAuditEntry): string | null {
  if (!RELEASE_ACTIONS.has(entry.action)) return null
  const details = isRecord(entry.details) ? entry.details : null
  if (details === null || entry.target === '') return null
  const releaseId =
    entry.action === 'release.rollback' ? positiveId(details.to) : positiveId(details.release_id)
  const baseId =
    entry.action === 'release.rollback'
      ? positiveId(details.from)
      : positiveId(details.base_release_id)
  if (releaseId === null || baseId === null) return null
  return `/config/releases/${encodeURIComponent(entry.target)}/${releaseId}?base=${baseId}`
}

watch(
  () => route.query,
  () => {
    void load()
  },
  { deep: true },
)

onMounted(load)
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
      <div class="ds-page" data-testid="audit-page">
        <PageHeader title="Audit">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <IonButton fill="clear" :disabled="loading" @click="load">Refresh</IonButton>
          </template>
        </PageHeader>

        <Toolbar class="audit__filters">
          <label class="audit__field">
            <span>Source</span>
            <select
              :value="filters.source"
              data-testid="audit-filter-source"
              @change="onSourceInput"
            >
              <option value="">Every source</option>
              <option v-for="source in AUDIT_SOURCES" :key="source" :value="source">
                {{ source }}
              </option>
            </select>
          </label>

          <label class="audit__field">
            <span>Actor</span>
            <input
              :value="filters.actor"
              placeholder="name or id"
              data-testid="audit-filter-actor"
              @change="onActorInput"
            />
          </label>

          <label class="audit__field">
            <span>From</span>
            <input
              type="datetime-local"
              :value="filters.from"
              data-testid="audit-filter-from"
              @change="onFromInput"
            />
          </label>

          <label class="audit__field">
            <span>To</span>
            <input
              type="datetime-local"
              :value="filters.to"
              data-testid="audit-filter-to"
              @change="onToInput"
            />
          </label>
        </Toolbar>

        <p
          v-if="degraded.length > 0"
          class="audit__degraded"
          role="status"
          data-testid="audit-degraded"
        >
          The trail is incomplete: the {{ degraded.join(', ') }} source{{
            degraded.length === 1 ? '' : 's'
          }}
          could not be read.
        </p>

        <div v-if="loading" class="audit__busy">
          <IonSpinner name="dots" />
          <p>Loading the audit log...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="load"
        />

        <EmptyState
          v-else-if="entries.length === 0"
          title="Nothing recorded"
          detail="No audited action matches these filters in this window."
        />

        <template v-else>
          <DataTable :columns="columns" :rows="entries" :row-key="entryKey">
            <template #cell-at="{ row }">
              <time :datetime="row.at" :title="relativeTime(row.at)">{{ timestamp(row.at) }}</time>
            </template>
            <template #cell-source="{ row }">
              <span class="audit__source">{{ row.source }}</span>
            </template>
            <template #cell-actor="{ row }">
              <span :title="row.actorId">{{ row.actorName }}</span>
            </template>
            <template #cell-action="{ row }">
              <span>{{ row.action }}</span>
              <RouterLink
                v-if="changesPath(row) !== null"
                class="audit__changes"
                data-testid="audit-changes-link"
                :to="changesPath(row) ?? ''"
              >
                View changes
              </RouterLink>
            </template>
            <template #cell-target="{ row }">
              <code>{{ row.target }}</code>
            </template>
            <template #cell-details="{ row }">
              <div class="audit__details">
                <button
                  v-if="hasDetails(row)"
                  type="button"
                  class="audit__expand"
                  data-testid="audit-expand"
                  :aria-expanded="isExpanded(row)"
                  @click="toggleDetails(row)"
                >
                  {{ isExpanded(row) ? 'Hide' : 'Show' }} details
                </button>
                <pre v-if="isExpanded(row)" class="audit__json" data-testid="audit-details">{{
                  prettyDetails(row)
                }}</pre>
              </div>
            </template>
          </DataTable>

          <IonButton
            v-if="cursor !== null"
            expand="block"
            class="audit__more"
            data-testid="audit-load-more"
            :disabled="loadingMore"
            @click="loadMore"
          >
            <IonSpinner v-if="loadingMore" name="dots" />
            <span v-else>Load older entries</span>
          </IonButton>
        </template>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.audit__filters {
  margin-bottom: var(--ds-space-4);
}

.audit__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.audit__field > span {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.audit__field input,
.audit__field select {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.audit__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.audit__degraded {
  background: var(--ds-warn-soft);
  border: 1px solid var(--ds-warn);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.audit__source {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  display: inline-block;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.audit__changes {
  color: var(--ds-accent);
  display: block;
  font-size: var(--ds-font-size-xs);
  margin-top: var(--ds-space-1);
  text-decoration: none;
}

.audit__changes:hover,
.audit__changes:focus-visible {
  text-decoration: underline;
}

.audit__details {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
}

.audit__expand {
  align-self: flex-start;
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.audit__json {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
  margin: 0;
  max-height: 18rem;
  overflow: auto;
  padding: var(--ds-space-2);
  white-space: pre-wrap;
  word-break: break-word;
}

.audit__more {
  margin-top: var(--ds-space-4);
}
</style>
