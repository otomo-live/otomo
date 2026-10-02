<script setup lang="ts">
import { IonButton, IonCard, IonCardContent, IonContent, IonPage } from '@ionic/vue'
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { sanitiseInternalPath } from '@/auth/paths'
import RoleGate from '@/components/RoleGate.vue'
import { PageHeader } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * Where a signed-in visitor lands when their roles do not reach what they asked
 * for. Separate from the 404 because "no such page" and "not for you" deserve
 * different sentences, and because this one can offer a way onward.
 */

const store = useSessionStore()
const route = useRoute()
const router = useRouter()

/** Echoed back so the message can name the area. Sanitised, and text only. */
const area = computed(() => sanitiseInternalPath(route.query.area))
const held = computed(() => (store.roles.length === 0 ? 'no roles' : store.roles.join(', ')))

async function signOut(): Promise<void> {
  await store.signOut()
  await router.replace('/login')
}
</script>

<template>
  <IonPage>
    <IonContent>
      <div class="denied">
        <IonCard class="denied__card">
          <IonCardContent>
            <PageHeader title="Not available to this account" />
            <p>
              You are signed in as <strong>{{ store.displayName }}</strong> with {{ held }}, which
              does not reach
              <code v-if="area !== null">{{ area }}</code>
              <span v-else>this area</span>.
            </p>
            <p class="denied__note">
              Ask an administrator if you need access. Signing in again will not change this.
            </p>

            <div class="denied__actions">
              <!-- Only offered to a role the dashboard route actually admits:
                   a link that the guard immediately refuses is worse than no
                   link at all. -->
              <RoleGate role="viewer">
                <IonButton router-link="/dashboard">Go to the dashboard</IonButton>
              </RoleGate>
              <IonButton fill="clear" color="medium" @click="signOut">Sign out</IonButton>
            </div>
          </IonCardContent>
        </IonCard>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.denied {
  display: flex;
  justify-content: center;
  padding: 8vh var(--ds-space-4) 0;
}

.denied__card {
  max-width: 36rem;
  width: 100%;
}

.denied__note {
  color: var(--ds-text-muted);
}

.denied__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-4);
}
</style>
