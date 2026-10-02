<script setup lang="ts">
import {
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonModal,
  IonSpinner,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import ReleaseDiffPanel from '@/config/ReleaseDiffPanel.vue'
import type { ManifestDiff } from '@/config/releaseDiff'

/**
 * The promote confirmation: the diff from the target head to the source head,
 * a required message, and the typed word when the target is live.
 *
 * Promotion copies the lower channel's head onto this one as a NEW release, so
 * the target head's id moves. The diff is target head -> source head: it is what
 * the promoted release would contain relative to what the channel now serves.
 * The parent loads the source head and owns the request; this dialog is the
 * message field and the live gate.
 */

const props = defineProps<{
  isOpen: boolean
  targetChannel: string
  sourceChannel: string
  /** The source head's id, for the copy line; null while it is being read. */
  sourceReleaseId: number | null
  diff: ManifestDiff | null
  loading: boolean
  error: string | null
  /** Promoting into live changes what players receive. */
  live: boolean
  pending: boolean
}>()

const emit = defineEmits<{ confirm: [message: string]; cancel: [] }>()

const message = ref('')
const typed = ref('')

watch(
  () => props.isOpen,
  (open) => {
    if (open) {
      message.value = ''
      typed.value = ''
    }
  },
)

const canConfirm = computed(
  () =>
    !props.pending &&
    !props.loading &&
    props.diff !== null &&
    message.value.trim() !== '' &&
    (!props.live || typed.value === 'live'),
)
</script>

<template>
  <IonModal :is-open="props.isOpen" @did-dismiss="emit('cancel')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>Promote {{ props.sourceChannel }} → {{ props.targetChannel }}</IonTitle>
        <IonButtons slot="end">
          <IonButton @click="emit('cancel')">Cancel</IonButton>
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent class="ion-padding">
      <div v-if="props.loading" class="promote__busy">
        <IonSpinner name="dots" />
        <p>Reading the {{ props.sourceChannel }} head...</p>
      </div>

      <p v-else-if="props.error !== null" class="promote__error" data-testid="promote-error">
        {{ props.error }}
      </p>

      <template v-else-if="props.diff !== null">
        <p data-testid="promote-summary">
          Promote release {{ props.sourceReleaseId ?? '?' }} of {{ props.sourceChannel }} to
          {{ props.targetChannel }}.
        </p>

        <ReleaseDiffPanel :diff="props.diff" />

        <label class="promote__field">
          <span>Message</span>
          <textarea
            v-model="message"
            class="promote__message"
            rows="3"
            aria-label="Promotion message"
            data-testid="promote-message"
            placeholder="Why is this being promoted?"
          />
        </label>
        <p v-if="message.trim() === ''" class="promote__hint" data-testid="promote-message-hint">
          A message is required.
        </p>

        <div v-if="props.live" class="promote__live" data-testid="promote-live-gate">
          <p class="promote__live-title">This changes what players receive</p>
          <label class="promote__field">
            <span>Type <code>live</code> to promote to live</span>
            <input
              v-model="typed"
              class="promote__input"
              type="text"
              aria-label="Type live to confirm"
              data-testid="promote-live-confirm"
              placeholder="Type live to promote to players"
            />
          </label>
        </div>
      </template>

      <div class="promote__actions">
        <button
          type="button"
          class="promote__confirm"
          :class="{ 'promote__confirm--danger': props.live }"
          :disabled="!canConfirm"
          data-testid="promote-confirm"
          @click="emit('confirm', message.trim())"
        >
          Promote to {{ props.targetChannel }}
        </button>
        <button
          type="button"
          class="promote__cancel"
          data-testid="promote-cancel"
          @click="emit('cancel')"
        >
          Cancel
        </button>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.promote__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
}

.promote__error {
  color: var(--ds-danger);
}

.promote__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
  margin-top: var(--ds-space-4);
}

.promote__input,
.promote__message {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
}

.promote__input {
  max-width: 20rem;
}

.promote__message {
  min-height: 5rem;
  resize: vertical;
}

.promote__hint {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0 0;
}

.promote__live {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-md);
  color: var(--ds-danger);
  margin-top: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.promote__live-title {
  font-weight: 600;
  margin: 0;
}

.promote__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-5);
}

.promote__confirm,
.promote__cancel {
  border: 1px solid var(--ds-accent);
  border-radius: var(--ds-radius-sm);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  min-height: 2.25rem;
  padding: var(--ds-space-2) var(--ds-space-4);
}

.promote__confirm {
  background: var(--ds-accent);
  color: var(--ds-accent-contrast);
}

.promote__confirm--danger {
  background: var(--ds-danger);
  border-color: var(--ds-danger);
  color: var(--ds-danger-contrast);
}

.promote__confirm:disabled {
  cursor: default;
  opacity: 0.6;
}

.promote__cancel {
  background: transparent;
  color: var(--ds-text);
}
</style>
