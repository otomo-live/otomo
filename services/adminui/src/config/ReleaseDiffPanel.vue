<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { CopyableId, DataTable, EmptyState, Panel } from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import type { ManifestDiff } from '@/config/releaseDiff'

/**
 * The body of a release diff: the changed namespaces and the changed packs, with
 * the unchanged namespaces collapsed.
 *
 * It is a component rather than two panels pasted into each page because two
 * pages show it: the release diff, reading two published releases, and the
 * composer's preview, diffing a draft selection against the channel head. They
 * must read the same, and a second copy is a place for them to drift.
 *
 * The diff is passed in already computed, so this is presentation only and a
 * test needs no server.
 */

const props = defineProps<{ diff: ManifestDiff }>()

const namespaceColumns: DataTableColumn[] = [
  { key: 'status', label: 'Change' },
  { key: 'namespace', label: 'Namespace' },
  { key: 'version', label: 'Version' },
]

const packColumns: DataTableColumn[] = [
  { key: 'status', label: 'Change' },
  { key: 'name', label: 'Pack' },
  { key: 'sha', label: 'SHA-256' },
  { key: 'size', label: 'Size (B)', align: 'end' },
]

const changedNamespaces = computed(() =>
  props.diff.namespaces.filter((entry) => entry.status !== 'unchanged'),
)

const unchangedNamespaces = computed(() =>
  props.diff.namespaces.filter((entry) => entry.status === 'unchanged'),
)

function shortSha(value: string): string {
  return value.length > 12 ? `${value.slice(0, 12)}…` : value
}
</script>

<template>
  <Panel title="Namespaces" class="release-diff__panel">
    <DataTable
      v-if="changedNamespaces.length > 0"
      :columns="namespaceColumns"
      :rows="changedNamespaces"
      row-key="namespace"
    >
      <template #cell-status="{ row }">
        <span class="release-diff__badge">{{ row.status }}</span>
      </template>
      <template #cell-namespace="{ row }">
        <RouterLink :to="`/config/namespaces/${encodeURIComponent(row.namespace)}`">
          {{ row.namespace }}
        </RouterLink>
      </template>
      <template #cell-version="{ row }">
        <span v-if="row.status === 'changed'">
          {{ row.beforeVersion }} → {{ row.afterVersion }}
        </span>
        <span v-else-if="row.status === 'added'">added (v{{ row.afterVersion }})</span>
        <span v-else-if="row.status === 'removed'">removed (was v{{ row.beforeVersion }})</span>
      </template>
    </DataTable>
    <EmptyState
      v-else
      title="No namespace changed"
      detail="This release carries the same namespace versions as its base."
    />

    <details
      v-if="unchangedNamespaces.length > 0"
      class="release-diff__unchanged"
      data-testid="release-unchanged"
    >
      <summary>
        {{ unchangedNamespaces.length }} unchanged namespace{{
          unchangedNamespaces.length === 1 ? '' : 's'
        }}
      </summary>
      <ul>
        <li v-for="entry in unchangedNamespaces" :key="entry.namespace">
          {{ entry.namespace }} (v{{ entry.afterVersion }})
        </li>
      </ul>
    </details>
  </Panel>

  <Panel title="Packs" class="release-diff__panel">
    <DataTable
      v-if="diff.packs.length > 0"
      :columns="packColumns"
      :rows="diff.packs"
      row-key="sha256"
    >
      <template #cell-status="{ row }">
        <span class="release-diff__badge">{{ row.status }}</span>
      </template>
      <template #cell-name="{ row }">{{ row.name }}</template>
      <template #cell-sha="{ row }">
        <template v-if="row.previousSha256">
          <CopyableId
            :value="row.previousSha256"
            :display="shortSha(row.previousSha256)"
            :label="`previous sha for ${row.name}`"
          />
          →
        </template>
        <CopyableId
          :value="row.sha256"
          :display="shortSha(row.sha256)"
          :label="`sha for ${row.name}`"
        />
      </template>
      <template #cell-size="{ row }">{{ row.size.toLocaleString() }}</template>
    </DataTable>
    <EmptyState
      v-else
      title="No pack changed"
      detail="This release carries the same packs as its base."
    />
  </Panel>
</template>

<style scoped>
.release-diff__panel {
  margin-top: var(--ds-space-4);
}

.release-diff__badge {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  display: inline-block;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.release-diff__unchanged {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin-top: var(--ds-space-3);
}

.release-diff__unchanged ul {
  margin: var(--ds-space-2) 0 0;
}
</style>
