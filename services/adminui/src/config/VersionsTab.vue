<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'

import { listVersions, type VersionSummary } from '@/api/config'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { CopyableId, DataTable, EmptyState, ErrorPanel } from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'

/**
 * The version history for one namespace, in the editor's third tab.
 *
 * It pages newest-first through `GET /versions`, and selecting a row is what
 * drives the diff below it. Selection is a pair at most, because a diff is
 * between exactly two documents: a second row replaces the older end. `Draft` is
 * a selectable pseudo-row, which is how "what is in the working draft compared
 * with version 11" is asked without an editor in between.
 *
 * The component owns only the list. The selection lives in the editor, because
 * it is URL state (`?from=11&to=draft`) and a deep link has to restore it before
 * the diff is fetched.
 */

const props = defineProps<{
  namespace: string
  from: string | null
  to: string | null
}>()

const emit = defineEmits<{ select: [ref: string] }>()

const versions = ref<VersionSummary[]>([])
const nextBefore = ref<number | null>(null)
const loading = ref(true)
const loadingMore = ref(false)
const failure = ref<FailureMessage | null>(null)

const columns: DataTableColumn[] = [
  { key: 'version', label: 'Version' },
  { key: 'message', label: 'Message' },
  { key: 'author', label: 'Author' },
  { key: 'created', label: 'Time' },
  { key: 'schema', label: 'Schema' },
  { key: 'sha', label: 'SHA-256' },
]

function isSelected(reference: string): boolean {
  return props.from === reference || props.to === reference
}

function formatTime(value: string): string {
  if (value === '') return ''
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? value : new Date(parsed).toLocaleString()
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const page = await listVersions(props.namespace)
    versions.value = page.versions
    nextBefore.value = page.nextBefore
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

async function loadMore(): Promise<void> {
  const before = nextBefore.value
  if (before === null || loadingMore.value) return
  loadingMore.value = true
  try {
    const page = await listVersions(props.namespace, { before })
    versions.value = [...versions.value, ...page.versions]
    nextBefore.value = page.nextBefore
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loadingMore.value = false
  }
}

function select(reference: string): void {
  emit('select', reference)
}

onMounted(load)
watch(() => props.namespace, load)
</script>

<template>
  <div class="versions">
    <p v-if="loading" class="versions__busy">Loading version history...</p>

    <ErrorPanel
      v-else-if="failure !== null"
      :message="failure.summary"
      :detail="failure.detail"
      :request-id="failure.reference"
      retry-label="Try again"
      @retry="load"
    />

    <template v-else>
      <p class="versions__hint">
        Select two rows to compare them, or a row and
        <button
          type="button"
          class="versions__draft"
          :aria-pressed="isSelected('draft')"
          :class="{ 'versions__draft--selected': isSelected('draft') }"
          data-testid="versions-draft"
          @click="select('draft')"
        >
          Draft
        </button>
        . The newest version is at the top.
      </p>

      <EmptyState
        v-if="versions.length === 0"
        title="No versions yet"
        detail="Creating the first version publishes the draft."
      />

      <DataTable
        v-else
        :columns="columns"
        :rows="versions"
        row-key="version"
        data-testid="versions-table"
        @row-click="(row) => select(String(row.version))"
      >
        <template #cell-version="{ row }">
          <span
            class="versions__version"
            :data-selected="isSelected(String(row.version)) ? 'true' : 'false'"
          >
            <span
              class="versions__marker"
              :class="{ 'versions__marker--on': isSelected(String(row.version)) }"
              aria-hidden="true"
            />
            v{{ row.version }}
          </span>
        </template>
        <template #cell-message="{ row }">
          <span class="versions__message">{{ row.message }}</span>
        </template>
        <template #cell-author="{ row }">{{ row.createdBy }}</template>
        <template #cell-created="{ row }">{{ formatTime(row.createdAt) }}</template>
        <template #cell-schema="{ row }">v{{ row.schemaVersion }}</template>
        <template #cell-sha="{ row }">
          <CopyableId
            :value="row.sha256"
            :display="row.sha256.slice(0, 12)"
            :label="`version ${row.version} SHA-256`"
          />
        </template>
      </DataTable>

      <div v-if="nextBefore !== null" class="versions__more">
        <button
          type="button"
          class="versions__more-button"
          :disabled="loadingMore"
          data-testid="versions-load-more"
          @click="loadMore"
        >
          {{ loadingMore ? 'Loading...' : 'Load more' }}
        </button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.versions {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-3);
}

.versions__busy,
.versions__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.versions__draft {
  background: var(--ds-neutral-soft);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  cursor: pointer;
  font: inherit;
  padding: 0 var(--ds-space-1);
}

.versions__draft--selected {
  background: var(--ds-info-soft);
  border-color: var(--ds-info);
  color: var(--ds-info);
}

.versions__version {
  align-items: center;
  display: inline-flex;
  gap: var(--ds-space-2);
  white-space: nowrap;
}

.versions__marker {
  background: var(--ds-border);
  border-radius: 50%;
  display: inline-block;
  height: 0.55rem;
  width: 0.55rem;
}

.versions__marker--on {
  background: var(--ds-accent);
}

.versions__message {
  overflow-wrap: anywhere;
}

.versions__more {
  display: flex;
  justify-content: center;
}

.versions__more-button {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2) var(--ds-space-4);
}

.versions__more-button:disabled {
  cursor: default;
  opacity: 0.7;
}
</style>
