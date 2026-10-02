<script setup lang="ts">
import { IonButton, IonContent, IonHeader, IonPage, IonToolbar } from '@ionic/vue'

import { EmptyState, PageHeader } from '@/components/ui'
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar />
    </IonHeader>
    <IonContent>
      <div class="ds-page">
        <PageHeader title="Not found" />
        <EmptyState title="No such page.">
          <template #actions>
            <IonButton router-link="/dashboard" router-direction="forward">
              Go to dashboard
            </IonButton>
          </template>
        </EmptyState>
      </div>
    </IonContent>
  </IonPage>
</template>
