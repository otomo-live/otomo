<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonModal,
  IonSpinner,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'

/**
 * A confirmation modal with one consequential action. Revoking a pending link
 * and resetting a second factor are both irreversible enough to ask twice, and
 * both only need the message and a label, so they share this rather than two
 * near-identical dialogs.
 */

withDefaults(
  defineProps<{
    isOpen: boolean
    title: string
    message: string
    confirmLabel: string
    /** Paint the confirm button as destructive. */
    danger?: boolean
    pending?: boolean
  }>(),
  { danger: false, pending: false },
)

const emit = defineEmits<{ confirm: []; cancel: [] }>()
</script>

<template>
  <IonModal :is-open="isOpen" data-testid="confirm-dialog" @did-dismiss="emit('cancel')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>{{ title }}</IonTitle>
      </IonToolbar>
    </IonHeader>
    <IonContent class="ion-padding">
      <p class="confirm__message" data-testid="confirm-dialog-message">{{ message }}</p>
      <div class="confirm__actions">
        <IonButton fill="clear" color="medium" type="button" @click="emit('cancel')">
          Cancel
        </IonButton>
        <IonButton
          :color="danger ? 'danger' : 'primary'"
          :disabled="pending"
          data-testid="confirm-dialog-confirm"
          @click="emit('confirm')"
        >
          <IonSpinner v-if="pending" name="dots" />
          <span v-else>{{ confirmLabel }}</span>
        </IonButton>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.confirm__message {
  margin: 0;
}

.confirm__actions {
  display: flex;
  gap: var(--ds-space-2);
  justify-content: flex-end;
  margin-top: var(--ds-space-4);
}
</style>
