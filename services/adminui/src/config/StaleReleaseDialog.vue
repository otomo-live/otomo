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

/**
 * The composer's 409 path: someone published to the channel since its head was
 * read.
 *
 * A publish names the head it previewed as `base_release_id`; when the channel
 * has moved on, Config refuses it with `409 stale_release` rather than publishing
 * content the reviewer never saw. It is a refusal, not an error, and there are
 * two honest things to do: read the new head and review the selections against
 * it (kept, not discarded), or back out. Neither happens on its own.
 */

const props = defineProps<{
  isOpen: boolean
  channel: string
  /** The head the composer previewed. */
  from: number
  /** The head the channel is now on. */
  to: number
}>()

const emit = defineEmits<{ review: []; cancel: [] }>()
</script>

<template>
  <IonModal :is-open="props.isOpen" @did-dismiss="emit('cancel')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>This channel has moved</IonTitle>
        <IonButtons slot="end">
          <IonButton @click="emit('cancel')">Cancel</IonButton>
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent class="ion-padding">
      <p data-testid="stale-release-message">
        Someone published to {{ props.channel }} since you opened this (release {{ props.from }} →
        {{ props.to }}).
      </p>
      <p class="stale__note">
        Your selections are kept. Reviewing reloads the new head and recomputes the diff against it;
        nothing has been published.
      </p>

      <div class="stale__actions">
        <IonButton expand="block" data-testid="stale-review" @click="emit('review')">
          Review the new head
        </IonButton>
        <IonButton expand="block" fill="outline" data-testid="stale-cancel" @click="emit('cancel')">
          Cancel
        </IonButton>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.stale__note {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.stale__actions {
  margin-top: var(--ds-space-5);
}
</style>
