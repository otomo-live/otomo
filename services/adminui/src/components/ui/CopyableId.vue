<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'

/**
 * A monospace id with a copy button.
 *
 * The clipboard API is only present in a secure context and can still be denied,
 * so a missing clipboard degrades to an unlabelled failure rather than a thrown
 * one: the id is still selectable text, which is the thing that matters.
 */

const props = defineProps<{
  value: string
  /** What the id is, for the copy button's accessible name. */
  label?: string
  /**
   * A shorter thing to show while the copy still writes the full `value`. A
   * 64-char hash in a table cell is unreadable, but copying a truncated one is
   * useless, so display and value are allowed to differ.
   */
  display?: string
}>()

const copied = ref(false)
let timer: ReturnType<typeof setTimeout> | null = null

function clearTimer(): void {
  if (timer !== null) {
    clearTimeout(timer)
    timer = null
  }
}

async function copy(): Promise<void> {
  try {
    const clipboard = navigator.clipboard
    if (clipboard === undefined || typeof clipboard.writeText !== 'function') return
    await clipboard.writeText(props.value)
  } catch {
    return
  }

  copied.value = true
  clearTimer()
  timer = setTimeout(() => {
    copied.value = false
    timer = null
  }, 1500)
}

onBeforeUnmount(clearTimer)
</script>

<template>
  <span class="copyable-id">
    <code class="copyable-id__value">{{ display ?? value }}</code>
    <button
      type="button"
      class="copyable-id__copy"
      :aria-label="`Copy ${label ?? value}`"
      @click="copy"
    >
      {{ copied ? 'Copied' : 'Copy' }}
    </button>
  </span>
</template>

<style scoped>
.copyable-id {
  align-items: center;
  display: inline-flex;
  gap: var(--ds-space-2);
  max-width: 100%;
}

.copyable-id__value {
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  overflow-wrap: anywhere;
}

.copyable-id__copy {
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.copyable-id__copy:hover {
  color: var(--ds-text);
}
</style>
