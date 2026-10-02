<script setup lang="ts">
import { computed } from 'vue'

/**
 * An auto-filling grid of cards or tiles.
 *
 * `min` is the smallest a column may be before the grid drops to fewer columns,
 * and the `min(100%, …)` in the track means a single column can still shrink
 * below it on a 400px screen without the page scrolling sideways.
 */

const props = withDefaults(
  defineProps<{
    /** Minimum column width in pixels. */
    min?: number
    /** A `--ds-space-*` step, 1 through 8. */
    gap?: number
  }>(),
  { min: 280, gap: 4 },
)

const columnTemplate = computed(() => `repeat(auto-fill, minmax(min(100%, ${props.min}px), 1fr))`)
const gapValue = computed(() => `var(--ds-space-${props.gap})`)
</script>

<template>
  <div
    class="grid"
    :style="{ gridTemplateColumns: columnTemplate, gap: gapValue }"
    data-testid="grid"
  >
    <slot />
  </div>
</template>

<style scoped>
.grid {
  display: grid;
  width: 100%;
}
</style>
