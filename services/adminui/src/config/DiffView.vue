<script setup lang="ts">
import { computed, ref } from 'vue'

import RawJsonEditor from '@/config/RawJsonEditor.vue'
import { formatPointerPath, opLabel, topLevelKey, type DiffChange } from '@/config/diff'
import { serialise } from '@/config/document'

/**
 * The structural diff between two documents, grouped for a human.
 *
 * The changes arrive in Config's wire vocabulary (`{op, path, from, to}`), one
 * row per leaf, and are grouped by their top-level key so a reader sees "the
 * `weapons` table moved" rather than an undifferentiated list of pointers. The
 * component is presentational: whoever rendered it fetched the two documents and
 * their diff, so the same view serves the version history and the create-version
 * dialog's draft-vs-latest preview without either one owning a second copy.
 *
 * `Side by side` swaps the change list for the two full documents in read-only
 * editors. The toggle is only offered when the caller supplied both documents;
 * a preview with changes alone can still render the list.
 */

const props = withDefaults(
  defineProps<{
    changes: DiffChange[]
    fromRef: string
    toRef: string
    fromDocument?: unknown
    toDocument?: unknown
    /** Tighter spacing for the create-version dialog's preview. */
    compact?: boolean
  }>(),
  { compact: false, fromDocument: undefined, toDocument: undefined },
)

interface DiffGroup {
  key: string
  changes: DiffChange[]
}

const sideBySide = ref(false)

const canCompare = computed(
  () => props.fromDocument !== undefined || props.toDocument !== undefined,
)

/** Grouped by the first path segment, groups in first-seen order. */
const groups = computed<DiffGroup[]>(() => {
  const byKey = new Map<string, DiffGroup>()
  for (const change of props.changes) {
    const key = topLevelKey(change.path)
    const group = byKey.get(key)
    if (group === undefined) byKey.set(key, { key, changes: [change] })
    else group.changes.push(change)
  }
  return [...byKey.values()]
})

/** A value as pretty JSON, or a word when it is absent. */
function pretty(value: unknown): string {
  if (value === undefined) return 'not set'
  const text = JSON.stringify(value, null, 2)
  return text === undefined ? String(value) : text
}

/** A value's one-line preview. Objects get a compact JSON text. */
function preview(value: unknown): string {
  if (value === undefined) return 'not set'
  const text = JSON.stringify(value)
  return text === undefined ? String(value) : text
}

/** Whether a value is long enough that the row should offer an expand. */
function isLong(value: unknown): boolean {
  const text = pretty(value)
  return text.length > 120 || text.includes('\n')
}

function opStatus(op: string): string {
  if (op === 'add') return 'add'
  if (op === 'remove') return 'remove'
  return 'change'
}
</script>

<template>
  <div class="diff" :class="{ 'diff--compact': props.compact }" data-testid="diff-view">
    <p v-if="props.changes.length === 0" class="diff__empty" data-testid="diff-empty">
      No differences.
    </p>

    <template v-else>
      <div class="diff__toolbar">
        <p class="diff__summary" data-testid="diff-summary">
          {{ props.changes.length }} change{{ props.changes.length === 1 ? '' : 's' }} from
          <code>{{ props.fromRef }}</code> to <code>{{ props.toRef }}</code>
        </p>
        <label v-if="canCompare" class="diff__toggle">
          <input v-model="sideBySide" type="checkbox" data-testid="diff-side-by-side" />
          Side by side
        </label>
      </div>

      <div v-if="sideBySide && canCompare" class="diff__panes" data-testid="diff-panes">
        <div class="diff__pane">
          <h3 class="diff__pane-title">{{ props.fromRef }}</h3>
          <RawJsonEditor
            :model-value="serialise(props.fromDocument)"
            :readonly="true"
            :aria-label="`${props.fromRef} document`"
          />
        </div>
        <div class="diff__pane">
          <h3 class="diff__pane-title">{{ props.toRef }}</h3>
          <RawJsonEditor
            :model-value="serialise(props.toDocument)"
            :readonly="true"
            :aria-label="`${props.toRef} document`"
          />
        </div>
      </div>

      <div v-else class="diff__groups">
        <section
          v-for="group in groups"
          :key="group.key"
          class="diff__group"
          data-testid="diff-group"
        >
          <h3 class="diff__group-title">{{ group.key }}</h3>
          <ul class="diff__list">
            <li
              v-for="change in group.changes"
              :key="`${change.op}:${change.path}`"
              class="diff__row"
              data-testid="diff-row"
            >
              <span class="diff__path" :title="change.path">{{
                formatPointerPath(change.path)
              }}</span>
              <span
                class="diff__badge"
                :class="`diff__badge--${opStatus(change.op)}`"
                data-testid="diff-op"
                >{{ opLabel(change.op) }}</span
              >
              <span class="diff__values">
                <template v-if="change.op === 'add'">
                  <span class="diff__absent">&mdash;</span>
                  <span class="diff__arrow">&rarr;</span>
                  <span class="diff__value">{{ preview(change.to) }}</span>
                </template>
                <template v-else-if="change.op === 'remove'">
                  <span class="diff__value">{{ preview(change.from) }}</span>
                  <span class="diff__arrow">&rarr;</span>
                  <span class="diff__absent">&mdash;</span>
                </template>
                <template v-else>
                  <span class="diff__value">{{ preview(change.from) }}</span>
                  <span class="diff__arrow">&rarr;</span>
                  <span class="diff__value">{{ preview(change.to) }}</span>
                </template>
              </span>

              <details v-if="isLong(change.from) || isLong(change.to)" class="diff__expand">
                <summary>Full values</summary>
                <div class="diff__expanded">
                  <pre v-if="change.from !== undefined" class="diff__pre">{{
                    pretty(change.from)
                  }}</pre>
                  <pre v-if="change.to !== undefined" class="diff__pre">{{
                    pretty(change.to)
                  }}</pre>
                </div>
              </details>
            </li>
          </ul>
        </section>
      </div>
    </template>
  </div>
</template>

<style scoped>
.diff {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-4);
}

.diff__empty {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.diff__toolbar {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-3);
  justify-content: space-between;
}

.diff__summary {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.diff__toggle {
  align-items: center;
  color: var(--ds-text-muted);
  display: inline-flex;
  font-size: var(--ds-font-size-sm);
  gap: var(--ds-space-1);
}

.diff__groups {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-4);
}

.diff__group-title {
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-1);
}

.diff__list {
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  list-style: none;
  margin: 0;
  overflow: hidden;
  padding: 0;
}

.diff__row {
  align-items: baseline;
  border-top: 1px solid var(--ds-border);
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.diff__row:first-child {
  border-top: 0;
}

.diff__path {
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
}

.diff__badge {
  border-radius: var(--ds-radius-sm);
  font-size: var(--ds-font-size-xs);
  padding: 0 var(--ds-space-1);
  text-transform: uppercase;
}

.diff__badge--add {
  background: var(--ds-ok-soft);
  color: var(--ds-ok);
}

.diff__badge--remove {
  background: var(--ds-danger-soft);
  color: var(--ds-danger);
}

.diff__badge--change {
  background: var(--ds-warn-soft);
  color: var(--ds-warn);
}

.diff__values {
  align-items: baseline;
  color: var(--ds-text-muted);
  display: inline-flex;
  flex-wrap: wrap;
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  gap: var(--ds-space-1);
}

.diff__value {
  color: var(--ds-text);
}

.diff__absent {
  color: var(--ds-text-muted);
}

.diff__expand {
  flex-basis: 100%;
  font-size: var(--ds-font-size-xs);
}

.diff__expand summary {
  color: var(--ds-accent);
  cursor: pointer;
}

.diff__expanded {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-2);
}

.diff__pre {
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  margin: 0;
  max-height: 16rem;
  overflow: auto;
  padding: var(--ds-space-2);
  white-space: pre-wrap;
}

.diff__panes {
  display: grid;
  gap: var(--ds-space-3);
  grid-template-columns: 1fr;
}

@media (min-width: 1024px) {
  .diff__panes {
    grid-template-columns: 1fr 1fr;
  }
}

.diff__pane-title {
  color: var(--ds-text-muted);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-1);
}

.diff--compact {
  gap: var(--ds-space-2);
}
</style>
