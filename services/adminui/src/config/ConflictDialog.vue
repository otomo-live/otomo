<script setup lang="ts">
import {
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonItem,
  IonLabel,
  IonList,
  IonModal,
  IonNote,
  IonSpinner,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'

import type { DocumentChange } from '@/config/diff'
import { formatPointer, describeValue } from '@/config/diff'

/**
 * The 409 path: someone else saved this draft first.
 *
 * Config's save is `UPDATE ... WHERE revision = $2` and zero rows updated is a
 * 409 `stale_revision` (CFG-B3). It is a refusal, not an error: the draft is
 * fine, this copy of it is simply out of date, and there are exactly two honest
 * things to do about that. Both are here, and neither is taken automatically.
 * Overwriting on the SPA's own initiative would discard a colleague's work
 * because of a race between two tabs, and reloading on its own would discard
 * what the person at the keyboard has been typing.
 *
 * So the dialog says what is at stake: what the draft moved to, and the changes
 * this editor is holding that the two ways forward treat differently. The change
 * list is the LOCAL diff, not the server's, because the question a reader has
 * here is "what am I about to lose", and only the editor can answer that.
 */

const props = withDefaults(
  defineProps<{
    isOpen: boolean
    localRevision: number
    serverRevision: number
    serverUpdatedBy: string
    serverUpdatedAt: string
    /** The unsaved changes in this editor, which is what an overwrite discards. */
    changes: DocumentChange[]
    /** A save is in flight, so both buttons are unavailable. */
    pending: boolean
    /**
     * The three strings a caller can restate. A create-version conflict is the
     * same two-way question as a save conflict, but "save mine over it" is not
     * what the non-reload button does there, so the wording is overridable
     * rather than hard-coded.
     */
    title?: string
    reloadLabel?: string
    keepLabel?: string
  }>(),
  {
    title: 'Someone else saved this draft',
    reloadLabel: 'Load the newer draft',
    keepLabel: 'Save mine over it',
  },
)

const emit = defineEmits<{
  dismiss: []
  reload: []
  overwrite: []
}>()
</script>

<template>
  <IonModal :is-open="props.isOpen" @did-dismiss="emit('dismiss')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>{{ props.title }}</IonTitle>
        <IonButtons slot="end">
          <IonButton @click="emit('dismiss')">Cancel</IonButton>
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent class="ion-padding">
      <p>
        You are editing revision {{ props.localRevision }}. The draft is now at revision
        {{ props.serverRevision }}, last saved by {{ props.serverUpdatedBy }} at
        {{ props.serverUpdatedAt }}. Nothing has been saved.
      </p>

      <h2 class="conflict__heading">Your unsaved changes ({{ props.changes.length }})</h2>
      <IonNote>
        Reloading discards these. Saving over the newer draft keeps them and replaces whatever was
        saved in the meantime.
      </IonNote>

      <IonList v-if="props.changes.length > 0">
        <IonItem v-for="change in props.changes" :key="`${change.kind}:${change.pointer}`">
          <IonLabel class="ion-text-wrap">
            <div class="conflict__pointer">{{ formatPointer(change.pointer) }}</div>
            <IonNote>
              <span v-if="change.kind === 'added'">added {{ describeValue(change.after) }}</span>
              <span v-else-if="change.kind === 'removed'">
                removed {{ describeValue(change.before) }}
              </span>
              <span v-else>
                {{ describeValue(change.before) }} &rarr; {{ describeValue(change.after) }}
              </span>
            </IonNote>
          </IonLabel>
        </IonItem>
      </IonList>
      <IonNote v-else>
        This editor has no unsaved changes, so the two options are equivalent.
      </IonNote>

      <div class="conflict__actions">
        <IonButton expand="block" :disabled="props.pending" @click="emit('reload')">
          <IonSpinner v-if="props.pending" name="dots" />
          <span v-else>{{ props.reloadLabel }}</span>
        </IonButton>
        <!-- Colour danger because this is the destructive one: it replaces a
             colleague's saved work with a document this editor is holding. -->
        <IonButton
          expand="block"
          color="danger"
          fill="outline"
          :disabled="props.pending"
          @click="emit('overwrite')"
        >
          {{ props.keepLabel }}
        </IonButton>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.conflict__heading {
  font-size: 1rem;
  margin-bottom: 0.25rem;
}

.conflict__pointer {
  font-family: var(--app-font-mono);
  font-size: 0.85rem;
}

.conflict__actions {
  margin-top: 1.5rem;
}
</style>
