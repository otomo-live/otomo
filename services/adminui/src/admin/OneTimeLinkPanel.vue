<script setup lang="ts">
import { computed } from 'vue'

import type { OneTimeLink } from '@/api/users'
import { CopyableId } from '@/components/ui'

/**
 * The one-time invite / reset link.
 *
 * The server returns the URL exactly once and never again, so this panel is the
 * only place it exists. Closing the dialog that contains it discards the value;
 * the parent must not keep a copy anywhere a later render could reach. The
 * warning says as much in words, because "copy this now" is the whole point.
 */

const props = defineProps<{
  link: OneTimeLink
  /** What the link does, e.g. "Invite link" or "Password reset link". */
  title: string
}>()

function expiryText(value: string): string {
  const at = Date.parse(value)
  if (Number.isNaN(at)) return value
  const hours = Math.max(1, Math.round((at - Date.now()) / 3_600_000))
  return `expires in ${hours} h, at ${new Date(at).toLocaleString()}`
}

const expiry = computed(() => expiryText(props.link.expiresAt))
</script>

<template>
  <div class="one-time" data-testid="one-time-link">
    <h3 class="one-time__title">{{ props.title }}</h3>
    <CopyableId :value="props.link.url" label="one-time link" data-testid="one-time-link-value" />
    <p class="one-time__expiry" data-testid="one-time-link-expiry">{{ expiry }}</p>
    <p class="one-time__warning" data-testid="one-time-link-warning">
      This link is shown once. Send it privately; anyone with it can create this account.
    </p>
  </div>
</template>

<style scoped>
.one-time {
  background: var(--ds-warn-soft);
  border: 1px solid var(--ds-warn);
  border-radius: var(--ds-radius-md);
  padding: var(--ds-space-4);
}

.one-time__title {
  font-size: var(--ds-font-size-md);
  font-weight: 600;
  margin: 0 0 var(--ds-space-2);
}

.one-time__expiry {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
}

.one-time__warning {
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
  margin: var(--ds-space-2) 0 0;
}
</style>
