<script setup lang="ts">
import StatusDot from '@/components/ui/StatusDot.vue'
import type { StatusKind } from '@/components/ui/types'

/**
 * One headline number with its label, unit and optional status.
 *
 * The value is the loud thing and the label is the quiet one, which is the
 * opposite of a table row: a dashboard tile is scanned, not read.
 */

defineProps<{
  label: string
  value: string | number
  unit?: string
  status?: StatusKind
  hint?: string
}>()
</script>

<template>
  <div class="stat-tile">
    <p class="stat-tile__label">{{ label }}</p>
    <p class="stat-tile__value">
      <span class="stat-tile__number">{{ value }}</span>
      <span v-if="unit !== undefined" class="stat-tile__unit">{{ unit }}</span>
    </p>
    <StatusDot v-if="status !== undefined" :status="status" />
    <p v-if="hint !== undefined" class="stat-tile__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.stat-tile {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  box-shadow: var(--ds-shadow-1);
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
  padding: var(--ds-space-4);
}

.stat-tile__label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.stat-tile__value {
  align-items: baseline;
  display: flex;
  gap: var(--ds-space-1);
  margin: 0;
}

.stat-tile__number {
  color: var(--ds-text);
  font-size: var(--ds-font-size-2xl);
  font-weight: 600;
  line-height: var(--ds-line-height-tight);
}

.stat-tile__unit {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.stat-tile__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  margin: 0;
}
</style>
