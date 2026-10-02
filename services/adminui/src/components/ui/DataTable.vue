<script setup lang="ts" generic="T">
import type { DataTableColumn } from '@/components/ui/types'

/**
 * A semantic table with a sticky header that scrolls inside its own box.
 *
 * It is generic over the row type, and cells are addressed by string key at
 * runtime rather than by the compiler: a caller supplies `columns` and `rows`,
 * and a column with a `cell-<key>` slot can render anything while every other
 * column falls back to the row property's text.
 *
 * The horizontal scroll is on the wrapper, never the page. A table wider than a
 * 400px phone must not make the document scroll sideways.
 */

const props = withDefaults(
  defineProps<{
    columns: DataTableColumn[]
    rows: T[]
    /**
     * A stable key for a row: a row property name, or a function when the
     * identity is not one property.
     */
    rowKey: string | ((row: T) => string | number)
    loading?: boolean
  }>(),
  { loading: false },
)

const emit = defineEmits<{ 'row-click': [row: T] }>()

function keyFor(row: T): string | number {
  if (typeof props.rowKey === 'function') return props.rowKey(row)
  const value = (row as unknown as Record<string, unknown>)[props.rowKey]
  return typeof value === 'number' ? value : String(value)
}

function valueFor(row: T, column: DataTableColumn): unknown {
  return (row as unknown as Record<string, unknown>)[column.key]
}

function display(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'object') {
    const text = JSON.stringify(value)
    return text === undefined ? String(value) : text
  }
  return String(value)
}

function alignFor(column: DataTableColumn): 'left' | 'center' | 'right' | undefined {
  if (column.align === 'center') return 'center'
  if (column.align === 'end') return 'right'
  if (column.align === 'start') return 'left'
  return undefined
}
</script>

<template>
  <div class="data-table" :aria-busy="loading">
    <div class="data-table__scroll">
      <table class="data-table__table">
        <thead>
          <tr>
            <th
              v-for="column in props.columns"
              :key="column.key"
              scope="col"
              :style="{ textAlign: alignFor(column), width: column.width }"
            >
              {{ column.label }}
            </th>
          </tr>
        </thead>
        <tbody v-if="props.rows.length > 0">
          <tr
            v-for="row in props.rows"
            :key="keyFor(row)"
            class="data-table__row"
            @click="emit('row-click', row)"
          >
            <td
              v-for="column in props.columns"
              :key="column.key"
              :class="{ 'data-table__cell--mono': column.mono }"
              :style="{ textAlign: alignFor(column) }"
            >
              <slot :name="`cell-${column.key}`" :row="row" :value="valueFor(row, column)">
                {{ display(valueFor(row, column)) }}
              </slot>
            </td>
          </tr>
        </tbody>
        <tbody v-else>
          <tr>
            <td :colspan="props.columns.length" class="data-table__empty">
              <slot name="empty">No rows.</slot>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-if="props.loading" class="data-table__loading" aria-live="polite">Loading...</p>
  </div>
</template>

<style scoped>
.data-table {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  overflow: hidden;
  position: relative;
}

.data-table__scroll {
  max-height: 70vh;
  overflow: auto;
  width: 100%;
}

.data-table__table {
  border-collapse: collapse;
  font-size: var(--ds-font-size-sm);
  width: 100%;
}

.data-table__table th,
.data-table__table td {
  border-bottom: 1px solid var(--ds-border);
  padding: var(--ds-space-2) var(--ds-space-3);
  text-align: left;
  vertical-align: top;
}

.data-table__table th {
  background: var(--ds-surface-raised);
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  letter-spacing: 0.04em;
  position: sticky;
  text-transform: uppercase;
  top: 0;
  z-index: 1;
}

.data-table__row {
  cursor: pointer;
}

.data-table__row:hover td {
  background: var(--ds-neutral-soft);
}

.data-table__cell--mono {
  font-family: var(--ds-font-mono);
  overflow-wrap: anywhere;
}

.data-table__empty {
  color: var(--ds-text-muted);
  padding: var(--ds-space-6) var(--ds-space-4) !important;
  text-align: center;
}

.data-table__loading {
  background: var(--ds-surface);
  border-top: 1px solid var(--ds-border);
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
  padding: var(--ds-space-2) var(--ds-space-3);
}
</style>
