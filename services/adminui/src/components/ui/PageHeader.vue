<script setup lang="ts">
/**
 * A page's title block: breadcrumb, title, optional subtitle and an actions slot.
 *
 * Shared so every view starts at the same place and the header carries the one
 * `<h1>` on the page.
 */

defineProps<{
  title: string
  subtitle?: string
}>()
</script>

<template>
  <header class="page-header">
    <div v-if="$slots.breadcrumb" class="page-header__breadcrumb">
      <slot name="breadcrumb" />
    </div>
    <div class="page-header__row">
      <div class="page-header__heading">
        <h1 class="page-header__title">{{ title }}</h1>
        <p v-if="subtitle !== undefined" class="page-header__subtitle">{{ subtitle }}</p>
      </div>
      <div v-if="$slots.actions" class="page-header__actions">
        <slot name="actions" />
      </div>
    </div>
  </header>
</template>

<style scoped>
.page-header {
  margin-bottom: var(--ds-space-5);
}

.page-header__breadcrumb {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin-bottom: var(--ds-space-2);
}

.page-header__row {
  align-items: flex-start;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-3);
  justify-content: space-between;
}

.page-header__title {
  color: var(--ds-text);
  font-size: var(--ds-font-size-2xl);
  line-height: var(--ds-line-height-tight);
  margin: 0;
}

.page-header__subtitle {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-md);
  line-height: var(--ds-line-height-normal);
  margin: var(--ds-space-1) 0 0;
}

.page-header__actions {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
}
</style>
