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
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { envConfig } from '@/api/env'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { HOME_PATH } from '@/auth/guard'
import { outcomeDestination } from '@/auth/outcome'
import { sanitiseInternalPath } from '@/auth/paths'
import { ErrorPanel, PageHeader } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * Staff sign-in.
 *
 * The outcome drives where this goes: `mfa_required` moves to the second-factor
 * screen and `mfa_enrollment_required` to TOTP setup, each with the ticket the
 * store is already holding. `authenticated` continues to what the visitor asked
 * for.
 */

const store = useSessionStore()
const route = useRoute()
const router = useRouter()

const appTitle = envConfig().appTitle
const email = ref('')
const password = ref('')
const pending = ref(false)
const failure = ref<FailureMessage | null>(null)

/** Set when a challenge ran out and sent the visitor back here. */
const expired = computed(() => route.query.expired === '1')

/** Where to go after signing in, if the guard sent us here from somewhere. */
const returnTo = computed(() => sanitiseInternalPath(route.query.returnTo) ?? HOME_PATH)

async function submit(): Promise<void> {
  if (pending.value) return
  pending.value = true
  failure.value = null

  try {
    const outcome = await store.signIn(email.value, password.value)
    await router.replace(outcomeDestination(outcome, returnTo.value))
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    // On every path, including the successful one. A password left in a field
    // is a password left on a screen that is about to be navigated away from.
    password.value = ''
    pending.value = false
  }
}
</script>

<template>
  <IonPage>
    <IonContent>
      <div class="signin">
        <IonCard class="signin__card">
          <IonCardContent>
            <PageHeader :title="appTitle" subtitle="Staff sign-in" />

            <p v-if="expired" class="signin__expired" data-testid="signin-expired" role="status">
              That setup took too long and expired. Sign in again to restart it.
            </p>

            <form @submit.prevent="submit">
              <!--
                The label is the input's own `label` prop rather than a
                sibling IonLabel. Both look the same, but only this one
                associates the text with the field: with a sibling label Ionic
                renders the text and leaves the native input with no accessible
                name at all, which is a screen-reader problem before it is a
                testing one.
              -->
              <IonList>
                <IonItem>
                  <IonInput
                    v-model="email"
                    label="Email"
                    label-placement="stacked"
                    type="email"
                    autocomplete="username"
                    inputmode="email"
                    required
                    :disabled="pending"
                  />
                </IonItem>
                <IonItem>
                  <IonInput
                    v-model="password"
                    label="Password"
                    label-placement="stacked"
                    type="password"
                    autocomplete="current-password"
                    required
                    :disabled="pending"
                  />
                </IonItem>
              </IonList>

              <ErrorPanel
                v-if="failure !== null"
                class="signin__failure"
                :message="failure.summary"
                :detail="failure.detail"
                :request-id="failure.reference"
              />

              <IonButton expand="block" type="submit" class="ion-margin-top" :disabled="pending">
                <IonSpinner v-if="pending" name="dots" />
                <span v-else>Sign in</span>
              </IonButton>
            </form>
          </IonCardContent>
        </IonCard>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.signin {
  display: flex;
  justify-content: center;
  padding: 8vh var(--ds-space-4) 0;
}

.signin__card {
  max-width: 28rem;
  width: 100%;
}

.signin__failure {
  margin-top: var(--ds-space-3);
}

.signin__expired {
  background: var(--ds-warn-soft);
  border: 1px solid var(--ds-warn);
  border-radius: var(--ds-radius-md);
  color: var(--ds-text);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
  padding: var(--ds-space-3);
}
</style>
