<script setup lang="ts">
/**
 * A horizontal row of filters and actions that wraps on a narrow screen.
 */
</script>

<template>
  <div class="toolbar">
    <slot />
  </div>
</template>

<style scoped>
.toolbar {
  align-items: flex-end;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-3);
}

.toolbar > :deep(*) {
  min-width: 0;
}
</style>
