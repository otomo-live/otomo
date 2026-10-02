<script setup lang="ts">
import { IonButton, IonContent, IonHeader, IonModal, IonTitle, IonToolbar } from '@ionic/vue'

import type { OneTimeLink } from '@/api/users'
import OneTimeLinkPanel from '@/admin/OneTimeLinkPanel.vue'

/**
 * A modal that shows a link once.
 *
 * It exists so the surrounding page can render the panel and then unmount it:
 * when `isOpen` goes false the parent clears the link from its state, and no
 * later read can bring it back, because the list endpoints never carry one.
 */

defineProps<{
  isOpen: boolean
  link: OneTimeLink
  title: string
}>()

const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <IonModal :is-open="isOpen" data-testid="one-time-link-dialog" @did-dismiss="emit('close')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>One-time link</IonTitle>
      </IonToolbar>
    </IonHeader>
    <IonContent class="ion-padding">
      <OneTimeLinkPanel :link="link" :title="title" />
      <div class="one-time-dialog__actions">
        <IonButton data-testid="one-time-link-close" @click="emit('close')">
          Done — hide this link
        </IonButton>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.one-time-dialog__actions {
  margin-top: var(--ds-space-4);
}
</style>
