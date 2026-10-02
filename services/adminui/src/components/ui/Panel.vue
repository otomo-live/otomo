<script setup lang="ts">
/**
 * A bordered surface with an optional title and actions.
 *
 * `padding` is opt-out because a `DataTable` inside a panel draws its own edges
 * and should be able to bleed to them.
 */

withDefaults(
  defineProps<{
    title?: string
    padding?: boolean
  }>(),
  { padding: true },
)
</script>

<template>
  <section class="panel">
    <header v-if="title !== undefined || $slots.actions" class="panel__head">
      <h2 v-if="title !== undefined" class="panel__title">{{ title }}</h2>
      <div v-if="$slots.actions" class="panel__actions">
        <slot name="actions" />
      </div>
    </header>
    <div class="panel__body" :class="{ 'panel__body--padded': padding }">
      <slot />
    </div>
  </section>
</template>

<style scoped>
.panel {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  box-shadow: var(--ds-shadow-1);
  overflow: hidden;
}

.panel__head {
  align-items: center;
  border-bottom: 1px solid var(--ds-border);
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-3);
  justify-content: space-between;
  padding: var(--ds-space-3) var(--ds-space-4);
}

.panel__title {
  color: var(--ds-text);
  font-size: var(--ds-font-size-lg);
  line-height: var(--ds-line-height-tight);
  margin: 0;
}

.panel__actions {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
}

.panel__body--padded {
  padding: var(--ds-space-4);
}
</style>
