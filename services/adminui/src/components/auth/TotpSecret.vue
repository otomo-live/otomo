<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'

/**
 * A TOTP secret in base32, grouped for hand entry, with a copy control.
 *
 * The grouping is display only; the ungrouped value is what the clipboard gets.
 * `testid` and `copyTestid` exist so the enrollment screen keeps its original
 * hook names while the account page can have its own.
 */

const props = defineProps<{
  secret: string
  testid?: string
  copyTestid?: string
}>()

const copied = ref(false)
let timer: ReturnType<typeof setTimeout> | null = null

const groups = computed(() => props.secret.match(/.{1,4}/g) ?? [])

async function copy(): Promise<void> {
  try {
    await navigator.clipboard?.writeText(props.secret)
    copied.value = true
    if (timer !== null) clearTimeout(timer)
    timer = setTimeout(() => {
      copied.value = false
    }, 1500)
  } catch {
    // Denied or unavailable: the text stays selectable on screen.
  }
}

onBeforeUnmount(() => {
  if (timer !== null) clearTimeout(timer)
})
</script>

<template>
  <div class="totp-secret" :data-testid="testid ?? 'totp-secret'">
    <code class="totp-secret__value">{{ groups.join(' ') }}</code>
    <button
      type="button"
      class="totp-secret__copy"
      :data-testid="copyTestid ?? 'totp-secret-copy'"
      aria-label="Copy secret"
      @click="copy"
    >
      {{ copied ? 'Copied' : 'Copy' }}
    </button>
  </div>
</template>

<style scoped>
.totp-secret {
  align-items: center;
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  justify-content: space-between;
  padding: var(--ds-space-3);
}

.totp-secret__value {
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  letter-spacing: 0.08em;
  overflow-wrap: anywhere;
}

.totp-secret__copy {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}
</style>
