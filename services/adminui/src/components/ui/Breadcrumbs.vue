<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import { routes } from '@/router'
import { breadcrumbs } from '@/router/nav'

/**
 * The current route's breadcrumb, walked through `meta.crumbParent`.
 *
 * Rendered inside `PageHeader`'s breadcrumb slot by each page, so there is one
 * implementation and a page cannot invent its own trail. A top-level page has a
 * single crumb, which is just its title, so the trail is hidden rather than
 * repeating the `<h1>` immediately below it.
 */

const route = useRoute()
const crumbs = computed(() => breadcrumbs(route, routes))
</script>

<template>
  <nav v-if="crumbs.length > 1" class="breadcrumbs" aria-label="Breadcrumb">
    <ol class="breadcrumbs__list">
      <li
        v-for="(crumb, index) in crumbs"
        :key="`${crumb.label}:${index}`"
        class="breadcrumbs__item"
      >
        <RouterLink v-if="crumb.to !== null" class="breadcrumbs__link" :to="crumb.to">
          {{ crumb.label }}
        </RouterLink>
        <span v-else class="breadcrumbs__current" aria-current="page">{{ crumb.label }}</span>
      </li>
    </ol>
  </nav>
</template>

<style scoped>
.breadcrumbs__list {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-1);
  list-style: none;
  margin: 0;
  padding: 0;
}

.breadcrumbs__item + .breadcrumbs__item::before {
  color: var(--ds-text-muted);
  content: '/';
  margin-right: var(--ds-space-1);
}

.breadcrumbs__link {
  color: var(--ds-accent);
  text-decoration: none;
}

.breadcrumbs__link:hover,
.breadcrumbs__link:focus-visible {
  text-decoration: underline;
}

.breadcrumbs__current {
  color: var(--ds-text);
}
</style>
