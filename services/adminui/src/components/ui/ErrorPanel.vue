<script setup lang="ts">
import { computed } from 'vue'

import CopyableId from '@/components/ui/CopyableId.vue'

/**
 * A failure block for the COM-5 shape.
 *
 * It accepts an `ApiError`, the raw `{ error: { code, message, request_id } }`
 * envelope, or the individual props, so a caller that already has a
 * `FailureMessage` can pass its pieces without re-deriving them. The request id
 * is the one part of a failure worth copying into a bug report, so it goes
 * through `CopyableId`.
 */

const props = defineProps<{
  /** An `ApiError`, a COM-5 envelope, or an object with message/code/request_id. */
  error?: unknown
  message?: string
  /** The server's own words, shown under a mapped summary when they differ. */
  detail?: string
  code?: string | null
  requestId?: string | null
  /** When set, a button with this label is rendered and emits `retry`. */
  retryLabel?: string
  /** Show a retry button with the default label. */
  retry?: boolean
}>()

const emit = defineEmits<{ retry: [] }>()

const retryText = computed(() => props.retryLabel ?? (props.retry === true ? 'Try again' : null))

function recordOf(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : null
}

const resolved = computed(() => {
  const source = recordOf(props.error)
  const nested = source !== null ? recordOf(source.error) : null
  const from = nested ?? source

  const readString = (key: string): string | null => {
    const value = from === null ? undefined : from[key]
    return typeof value === 'string' && value !== '' ? value : null
  }

  const message = props.message ?? readString('message') ?? ''
  const detail = props.detail ?? readString('detail') ?? readString('message') ?? ''
  const code = props.code ?? readString('code')
  const requestId = props.requestId ?? readString('request_id') ?? readString('requestId') ?? null

  return { message, detail, code, requestId }
})
</script>

<template>
  <div class="error-panel" role="alert">
    <p class="error-panel__message">{{ resolved.message }}</p>
    <p
      v-if="resolved.detail !== '' && resolved.detail !== resolved.message"
      class="error-panel__detail"
    >
      {{ resolved.detail }}
    </p>
    <p v-if="resolved.code !== null" class="error-panel__code">
      Code <code>{{ resolved.code }}</code>
    </p>
    <p v-if="resolved.requestId !== null" class="error-panel__request">
      Reference <CopyableId :value="resolved.requestId" label="reference" />
    </p>
    <button
      v-if="retryText !== null"
      type="button"
      class="error-panel__retry"
      @click="emit('retry')"
    >
      {{ retryText }}
    </button>
  </div>
</template>

<style scoped>
.error-panel {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-md);
  color: var(--ds-text);
  padding: var(--ds-space-4);
}

.error-panel__message {
  font-size: var(--ds-font-size-md);
  font-weight: 600;
  line-height: var(--ds-line-height-tight);
  margin: 0 0 var(--ds-space-1);
}

.error-panel__detail,
.error-panel__code,
.error-panel__request {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0 0;
}

.error-panel__retry {
  background: var(--ds-accent);
  border: 1px solid transparent;
  border-radius: var(--ds-radius-sm);
  color: var(--ds-accent-contrast);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  margin-top: var(--ds-space-3);
  padding: var(--ds-space-2) var(--ds-space-4);
}
</style>
