<script setup lang="ts">
import { computed } from 'vue'

import type { StatusKind } from '@/components/ui/types'

/**
 * A colour-coded status indicator that never relies on colour alone.
 *
 * Unless `compact` is set, the status (or the supplied `label`) is rendered as
 * text next to the dot, so the meaning survives a monochrome display or a reader
 * who cannot tell the roles apart. The `aria-label` is always the label or the
 * status name.
 */

const props = withDefaults(
  defineProps<{
    status: StatusKind
    label?: string
    /** Dot only, no visible text. The accessible name is still set. */
    compact?: boolean
  }>(),
  { compact: false },
)

const text = computed(() => props.label ?? props.status)
</script>

<template>
  <span class="status-dot" role="img" :aria-label="text">
    <span class="status-dot__dot" :class="`status-dot__dot--${status}`" aria-hidden="true" />
    <span v-if="!compact" class="status-dot__label">{{ text }}</span>
  </span>
</template>

<style scoped>
.status-dot {
  align-items: center;
  display: inline-flex;
  gap: var(--ds-space-1);
  line-height: var(--ds-line-height-tight);
}

.status-dot__dot {
  background: var(--ds-neutral);
  border-radius: 50%;
  display: inline-block;
  flex: none;
  height: 0.6rem;
  width: 0.6rem;
}

.status-dot__dot--ok {
  background: var(--ds-ok);
}

.status-dot__dot--warn {
  background: var(--ds-warn);
}

.status-dot__dot--danger {
  background: var(--ds-danger);
}

.status-dot__dot--info {
  background: var(--ds-info);
}

.status-dot__dot--neutral {
  background: var(--ds-neutral);
}

.status-dot__label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}
</style>
