<script setup lang="ts">
import {
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonModal,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import ReleaseDiffPanel from '@/config/ReleaseDiffPanel.vue'
import type { ManifestDiff } from '@/config/releaseDiff'

/**
 * The rollback confirmation: the diff from the channel head back to the release
 * the operator picked, and, for live, the typed word that gates it.
 *
 * Rollback does not publish anything new; it points the channel at an earlier
 * snapshot. That makes the diff read head -> target, which is what the channel
 * would serve after the move. The dialog is presentation and a gate only: the
 * view owns the request, so a `stale_release`/`no_changes` refusal is handled
 * once for both mutations.
 */

const props = defineProps<{
  isOpen: boolean
  channel: string
  /** The current head the rollback names as `base_release_id`. */
  headReleaseId: number
  /** The earlier release to move the head to. */
  targetReleaseId: number
  diff: ManifestDiff
  /** Rolling live back changes what players receive. */
  live: boolean
  pending: boolean
}>()

const emit = defineEmits<{ confirm: []; cancel: [] }>()

const typed = ref('')

watch(
  () => props.isOpen,
  (open) => {
    if (open) typed.value = ''
  },
)

const canConfirm = computed(() => !props.pending && (!props.live || typed.value === 'live'))
</script>

<template>
  <IonModal :is-open="props.isOpen" @did-dismiss="emit('cancel')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>Roll back {{ props.channel }}</IonTitle>
        <IonButtons slot="end">
          <IonButton @click="emit('cancel')">Cancel</IonButton>
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent class="ion-padding">
      <p data-testid="rollback-summary">
        Move {{ props.channel }} from release {{ props.headReleaseId }} back to release
        {{ props.targetReleaseId }}.
      </p>

      <ReleaseDiffPanel :diff="props.diff" />

      <div v-if="props.live" class="rollback__live" data-testid="rollback-live-gate">
        <p class="rollback__live-title">This changes what players receive</p>
        <label class="rollback__live-label">
          <span>Type <code>live</code> to roll back live</span>
          <input
            v-model="typed"
            class="rollback__input"
            type="text"
            aria-label="Type live to confirm"
            data-testid="rollback-live-confirm"
            placeholder="Type live to roll back players"
          />
        </label>
      </div>

      <div class="rollback__actions">
        <button
          type="button"
          class="rollback__confirm"
          :class="{ 'rollback__confirm--danger': props.live }"
          :disabled="!canConfirm"
          data-testid="rollback-confirm"
          @click="emit('confirm')"
        >
          Roll back to {{ props.targetReleaseId }}
        </button>
        <button
          type="button"
          class="rollback__cancel"
          data-testid="rollback-cancel"
          @click="emit('cancel')"
        >
          Cancel
        </button>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.rollback__live {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-md);
  color: var(--ds-danger);
  margin-top: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.rollback__live-title {
  font-weight: 600;
  margin: 0 0 var(--ds-space-2);
}

.rollback__live-label {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.rollback__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  max-width: 20rem;
  padding: var(--ds-space-1) var(--ds-space-2);
}

.rollback__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-5);
}

.rollback__confirm,
.rollback__cancel {
  border: 1px solid var(--ds-accent);
  border-radius: var(--ds-radius-sm);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  min-height: 2.25rem;
  padding: var(--ds-space-2) var(--ds-space-4);
}

.rollback__confirm {
  background: var(--ds-accent);
  color: var(--ds-accent-contrast);
}

.rollback__confirm--danger {
  background: var(--ds-danger);
  border-color: var(--ds-danger);
  color: var(--ds-danger-contrast);
}

.rollback__confirm:disabled {
  cursor: default;
  opacity: 0.6;
}

.rollback__cancel {
  background: transparent;
  color: var(--ds-text);
}
</style>
