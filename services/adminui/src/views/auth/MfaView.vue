<script setup lang="ts">
import {
  IonButton,
  IonCard,
  IonCardContent,
  IonContent,
  IonInput,
  IonItem,
  IonList,
  IonPage,
  IonSpinner,
} from '@ionic/vue'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { describeFailure, type FailureMessage } from '@/api/messages'
import { HOME_PATH, LOGIN_PATH } from '@/auth/guard'
import { sanitiseInternalPath } from '@/auth/paths'
import { ErrorPanel, PageHeader } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * The second factor, reached only with a ticket from a login that asked for one.
 *
 * The ticket lives in the store's memory rather than in the URL, so a reload
 * loses it and this screen sends the visitor back to the password form. That is
 * the right failure: a ticket in a query string is one that lands in browser
 * history, in the referrer header and in any log along the way.
 */

const store = useSessionStore()
const route = useRoute()
const router = useRouter()

const code = ref('')
const pending = ref(false)
const failure = ref<FailureMessage | null>(null)

const ticketInHand = computed(() => store.mfaTicket !== null)
const returnTo = computed(() => sanitiseInternalPath(route.query.returnTo) ?? HOME_PATH)

onMounted(() => {
  if (!ticketInHand.value) void router.replace(LOGIN_PATH)
})

async function submit(): Promise<void> {
  if (pending.value || !ticketInHand.value) return
  pending.value = true
  failure.value = null

  try {
    await store.submitMfaCode(code.value)
    await router.replace(returnTo.value)
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    code.value = ''
    pending.value = false
  }
}

/** Abandons the challenge. `clear` is what drops the ticket, not this screen. */
async function backToSignIn(): Promise<void> {
  store.clear()
  await router.replace(LOGIN_PATH)
}
</script>

<template>
  <IonPage>
    <IonContent>
      <div class="mfa">
        <IonCard class="mfa__card">
          <IonCardContent>
            <!-- Nothing to render without a ticket: the mount hook is already
                 sending this visitor to the sign-in screen. -->
            <template v-if="ticketInHand">
              <PageHeader
                title="Second factor"
                subtitle="Enter the six-digit code from your authenticator, or one of your recovery codes."
              />

              <form @submit.prevent="submit">
                <!-- See LoginView: the label lives on the input so the field
                     has an accessible name. -->
                <IonList>
                  <IonItem>
                    <IonInput
                      v-model="code"
                      label="Code"
                      label-placement="stacked"
                      inputmode="numeric"
                      autocomplete="one-time-code"
                      required
                      :disabled="pending"
                    />
                  </IonItem>
                </IonList>

                <ErrorPanel
                  v-if="failure !== null"
                  class="mfa__failure"
                  :message="failure.summary"
                  :detail="failure.detail"
                  :request-id="failure.reference"
                />

                <IonButton expand="block" type="submit" class="ion-margin-top" :disabled="pending">
                  <IonSpinner v-if="pending" name="dots" />
                  <span v-else>Verify</span>
                </IonButton>
              </form>
            </template>

            <IonButton expand="block" fill="clear" @click="backToSignIn">Back to sign in</IonButton>
          </IonCardContent>
        </IonCard>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.mfa {
  display: flex;
  justify-content: center;
  padding: 8vh var(--ds-space-4) 0;
}

.mfa__card {
  max-width: 28rem;
  width: 100%;
}

.mfa__failure {
  margin-top: var(--ds-space-3);
}
</style>
