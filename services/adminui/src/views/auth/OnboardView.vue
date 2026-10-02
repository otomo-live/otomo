<script setup lang="ts">
import {
  IonButton,
  IonCard,
  IonCardContent,
  IonContent,
  IonInput,
  IonItem,
  IonList,
  IonNote,
  IonPage,
  IonSpinner,
} from '@ionic/vue'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import { onboardLookup, type OnboardLink } from '@/api/auth'
import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { HOME_PATH } from '@/auth/guard'
import { readFragmentToken, stripFragment } from '@/auth/onboard'
import { outcomeDestination } from '@/auth/outcome'
import PasswordStrengthMeter from '@/components/auth/PasswordStrengthMeter.vue'
import { ErrorPanel, PageHeader } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * Redeeming an onboarding link: `/onboard#token=…`.
 *
 * The token is read from the fragment and erased before it is used. It is never
 * put in a route, a query string or storage; the only place it lives after this
 * mount is a local variable that the lookup and the redeem send in a POST body.
 *
 * The page has four states: loading the link, the form, invalid/expired, and a
 * transport failure that is worth retrying. The form's password hints are only
 * hints: the server's 400 message is what the form shows when the policy
 * refuses.
 */

const store = useSessionStore()
const router = useRouter()

type Phase = 'loading' | 'form' | 'invalid' | 'error'

const phase = ref<Phase>('loading')
const token = ref<string | null>(null)
const link = ref<OnboardLink | null>(null)

const password = ref('')
const confirmation = ref('')
const pending = ref(false)
const failure = ref<FailureMessage | null>(null)

const mismatch = computed(() => confirmation.value !== '' && confirmation.value !== password.value)

const heading = computed(() => {
  if (link.value === null) return 'Set up your account'
  if (link.value.purpose === 'reset') return `Reset your password for ${link.value.email}`
  const role = link.value.role ?? 'staff'
  return `You've been invited to Otomo Admin as ${role}`
})

const expiry = computed(() => {
  if (link.value === null) return ''
  const at = new Date(link.value.expiresAt)
  return Number.isNaN(at.getTime()) ? '' : at.toLocaleString()
})

function isInvalidLink(error: unknown): boolean {
  return error instanceof ApiError && (error.code === 'invalid_link' || error.status === 404)
}

async function load(): Promise<void> {
  // Read and erase before anything is sent anywhere. `stripFragment` runs even
  // when there is no token, so a malformed link does not leave its fragment in
  // the address bar either.
  const found = readFragmentToken(window.location.hash)
  stripFragment(window.location, window.history)

  if (found === null) {
    phase.value = 'invalid'
    return
  }
  token.value = found

  try {
    link.value = await onboardLookup(found)
    phase.value = 'form'
  } catch (error) {
    if (isInvalidLink(error)) {
      phase.value = 'invalid'
      return
    }
    failure.value = describeFailure(error)
    phase.value = 'error'
  }
}

async function submit(): Promise<void> {
  if (pending.value || token.value === null) return
  if (password.value !== confirmation.value) return
  pending.value = true
  failure.value = null

  try {
    const outcome = await store.redeemOnboard(token.value, password.value)
    await router.replace(outcomeDestination(outcome, HOME_PATH))
  } catch (error) {
    // A link consumed or expired between the lookup and the redeem is the same
    // dead end as one that was never valid.
    if (isInvalidLink(error)) {
      phase.value = 'invalid'
      return
    }
    failure.value = describeFailure(error)
  } finally {
    password.value = ''
    confirmation.value = ''
    pending.value = false
  }
}

onMounted(load)
</script>

<template>
  <IonPage>
    <IonContent>
      <div class="onboard">
        <IonCard class="onboard__card">
          <IonCardContent>
            <template v-if="phase === 'loading'">
              <PageHeader title="Set up your account" />
              <div class="onboard__loading" data-testid="onboard-loading">
                <IonSpinner name="dots" />
                <span>Checking your link…</span>
              </div>
            </template>

            <template v-else-if="phase === 'invalid'">
              <PageHeader title="Link not valid" />
              <p class="onboard__invalid" data-testid="onboard-invalid">
                This link is invalid or has expired; ask an admin for a new one.
              </p>
            </template>

            <template v-else-if="phase === 'error'">
              <PageHeader title="Set up your account" />
              <ErrorPanel
                v-if="failure !== null"
                :message="failure.summary"
                :detail="failure.detail"
                :request-id="failure.reference"
                retry-label="Try again"
                @retry="load"
              />
            </template>

            <template v-else>
              <PageHeader :title="heading" :subtitle="link?.email ?? undefined" />

              <p v-if="expiry !== ''" class="onboard__expiry" data-testid="onboard-expiry">
                This link expires on {{ expiry }}.
              </p>

              <form @submit.prevent="submit">
                <IonList>
                  <IonItem>
                    <IonInput
                      v-model="password"
                      label="New password"
                      label-placement="stacked"
                      type="password"
                      autocomplete="new-password"
                      required
                      :disabled="pending"
                    />
                  </IonItem>
                  <IonItem>
                    <IonInput
                      v-model="confirmation"
                      label="Confirm password"
                      label-placement="stacked"
                      type="password"
                      autocomplete="new-password"
                      required
                      :disabled="pending"
                    />
                  </IonItem>
                </IonList>

                <PasswordStrengthMeter :password="password" :email="link?.email" />

                <IonNote v-if="mismatch" color="danger" class="onboard__mismatch">
                  The two passwords do not match.
                </IonNote>

                <ErrorPanel
                  v-if="failure !== null"
                  class="onboard__failure"
                  :message="failure.summary"
                  :detail="failure.detail"
                  :request-id="failure.reference"
                />

                <IonButton
                  expand="block"
                  type="submit"
                  class="ion-margin-top"
                  data-testid="onboard-submit"
                  :disabled="pending || mismatch || password.length === 0"
                >
                  <IonSpinner v-if="pending" name="dots" />
                  <span v-else>{{
                    link?.purpose === 'reset' ? 'Reset password' : 'Set password'
                  }}</span>
                </IonButton>
              </form>
            </template>
          </IonCardContent>
        </IonCard>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.onboard {
  display: flex;
  justify-content: center;
  padding: 8vh var(--ds-space-4) 0;
}

.onboard__card {
  max-width: 32rem;
  width: 100%;
}

.onboard__loading {
  align-items: center;
  color: var(--ds-text-muted);
  display: flex;
  gap: var(--ds-space-2);
}

.onboard__invalid {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-md);
  color: var(--ds-text);
  padding: var(--ds-space-4);
}

.onboard__expiry {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.onboard__strength {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-3);
}

.onboard__meter {
  display: flex;
  gap: var(--ds-space-1);
}

.onboard__bar {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  display: block;
  height: 6px;
  width: 36px;
}

.onboard__meter[data-score='1'] .onboard__bar--on,
.onboard__meter[data-score='2'] .onboard__bar--on {
  background: var(--ds-warn);
}

.onboard__meter[data-score='3'] .onboard__bar--on,
.onboard__meter[data-score='4'] .onboard__bar--on {
  background: var(--ds-ok);
}

.onboard__strength-label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.onboard__hints {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
  padding-left: var(--ds-space-5);
}

.onboard__mismatch {
  display: block;
  margin-top: var(--ds-space-2);
}

.onboard__failure {
  margin-top: var(--ds-space-3);
}
</style>
