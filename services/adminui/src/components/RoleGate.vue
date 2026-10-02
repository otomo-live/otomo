<script setup lang="ts">
import { computed } from 'vue'

import { hasRoleAtLeast } from '@/auth/roles'
import { useSessionStore } from '@/stores/session'

/**
 * Renders its default slot only when the session's roles reach `role`.
 *
 * An unknown status renders nothing, so gating is never decided before the
 * session is known: a control that flashes into view and then disappears reads
 * as a bug, and one that does the reverse is a lie for a frame.
 *
 * This is presentation, not enforcement. See `hasRoleAtLeast` for why that is
 * safe: the gateway refuses the request on its own.
 */

const props = defineProps<{ role: string }>()

const store = useSessionStore()

const allowed = computed(
  () => store.status === 'authenticated' && hasRoleAtLeast(store.roles, props.role),
)
</script>

<template>
  <slot v-if="allowed" />
  <slot v-else name="fallback" />
</template>
