<script setup lang="ts">
/**
 * "There is nothing to show, and that is not a failure."
 *
 * A list that renders nothing is ambiguous: was there nothing, or did something
 * break? A view that can legitimately have no rows says so with this, and a
 * failure says so with `ErrorPanel` rather than being folded into the same
 * blankness.
 *
 * The optional `actions` slot is where a way forward goes, when there is one.
 */

defineProps<{
  title: string
  detail?: string
}>()
</script>

<template>
  <div class="empty">
    <p class="empty__title">{{ title }}</p>
    <p v-if="detail !== undefined" class="empty__detail">{{ detail }}</p>
    <div v-if="$slots.actions" class="empty__actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<style scoped>
.empty {
  color: var(--ds-text-muted);
  padding: var(--ds-space-6) var(--ds-space-4);
  text-align: center;
}

.empty__title {
  color: var(--ds-text);
  font-size: var(--ds-font-size-md);
  font-weight: 600;
  line-height: var(--ds-line-height-tight);
  margin: 0 0 var(--ds-space-1);
}

.empty__detail {
  color: var(--ds-text-muted);
  margin: 0;
}

.empty__actions {
  margin-top: var(--ds-space-3);
}
</style>
