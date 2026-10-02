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
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { HOME_PATH, LOGIN_PATH } from '@/auth/guard'
import { sanitiseInternalPath } from '@/auth/paths'
import RecoveryCodes from '@/components/auth/RecoveryCodes.vue'
import TotpQrCode from '@/components/auth/TotpQrCode.vue'
import TotpSecret from '@/components/auth/TotpSecret.vue'
import { ErrorPanel, PageHeader } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * TOTP enrollment, reached from login (`mfa_enrollment_required`) or from
 * onboarding an admin invite, with the ticket already in the session store's
 * memory. Nothing about the challenge is in the URL, so a reload loses it and
 * this screen sends the visitor back to sign in; that is the right failure.
 *
 * Three steps: scan the QR, prove the first code, then save the recovery codes.
 * The codes screen is a dead end until the checkbox is ticked, because leaving
 * without them means losing the only second-factor fallback.
 */

const store = useSessionStore()
const route = useRoute()
const router = useRouter()

type Phase = 'loading' | 'scan' | 'recovery' | 'error'

const phase = ref<Phase>('loading')
const secret = ref('')
const otpauthUrl = ref('')
const code = ref('')
const codes = ref<string[]>([])
const saved = ref(false)
const pending = ref(false)
const failure = ref<FailureMessage | null>(null)

/** Ticks once a second so the ticket countdown is live. */
const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | null = null

const returnTo = computed(() => sanitiseInternalPath(route.query.returnTo) ?? HOME_PATH)

/** Seconds until the server ticket expires, or null when there is no deadline. */
const secondsLeft = computed(() => {
  const at = store.mfaTicketExpiresAt
  if (at === null) return null
  return Math.max(0, Math.ceil((at - now.value) / 1000))
})

function isInvalidTicket(error: unknown): boolean {
  return error instanceof ApiError && (error.code === 'invalid_ticket' || error.status === 401)
}

function startTimer(): void {
  stopTimer()
  timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
}

function stopTimer(): void {
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

async function backToSignIn(expired = false): Promise<void> {
  store.clear()
  await router.replace(expired ? { path: LOGIN_PATH, query: { expired: '1' } } : LOGIN_PATH)
}

async function loadSecret(): Promise<void> {
  try {
    const info = await store.beginEnrollment()
    secret.value = info.secret
    otpauthUrl.value = info.otpauthUrl
    phase.value = 'scan'
    now.value = Date.now()
    startTimer()
  } catch (error) {
    if (isInvalidTicket(error)) {
      await backToSignIn(true)
      return
    }
    failure.value = describeFailure(error)
    phase.value = 'error'
  }
}

async function confirm(): Promise<void> {
  if (pending.value) return
  pending.value = true
  failure.value = null

  try {
    codes.value = await store.confirmEnrollment(code.value)
    saved.value = false
    stopTimer()
    phase.value = 'recovery'
  } catch (error) {
    if (isInvalidTicket(error)) {
      await backToSignIn(true)
      return
    }
    failure.value = describeFailure(error)
  } finally {
    code.value = ''
    pending.value = false
  }
}

async function finish(): Promise<void> {
  await router.replace(returnTo.value)
}

onMounted(async () => {
  if (store.mfaTicket === null || store.mfaPurpose !== 'enroll') {
    await backToSignIn()
    return
  }
  await loadSecret()
})

// A challenge that runs out is not a failure to explain on this screen: the
// password step is the only place it can be renewed.
watch(secondsLeft, (remaining) => {
  if (phase.value === 'scan' && remaining === 0) void backToSignIn(true)
})

onBeforeUnmount(stopTimer)
</script>

<template>
  <IonPage>
    <IonContent>
      <div class="enroll">
        <IonCard class="enroll__card">
          <IonCardContent>
            <template v-if="phase === 'loading'">
              <PageHeader title="Set up two-factor authentication" />
              <div class="enroll__loading" data-testid="enroll-loading">
                <IonSpinner name="dots" />
                <span>Preparing your authenticator…</span>
              </div>
            </template>

            <template v-else-if="phase === 'error'">
              <PageHeader title="Set up two-factor authentication" />
              <ErrorPanel
                v-if="failure !== null"
                :message="failure.summary"
                :detail="failure.detail"
                :request-id="failure.reference"
                retry-label="Try again"
                @retry="loadSecret"
              />
            </template>

            <template v-else-if="phase === 'scan'">
              <PageHeader
                title="Set up two-factor authentication"
                subtitle="Admin accounts need an authenticator app before they can sign in."
              />

              <p
                v-if="secondsLeft !== null"
                class="enroll__countdown"
                data-testid="enroll-countdown"
              >
                This setup expires in {{ secondsLeft }}s.
              </p>

              <ol class="enroll__steps">
                <li>Add Otomo Admin to your authenticator app.</li>
                <li>Scan the QR code, or enter the secret by hand.</li>
                <li>Enter the six-digit code the app shows.</li>
              </ol>

              <div class="enroll__qr" data-testid="enroll-qr">
                <TotpQrCode :otpauth-url="otpauthUrl" />
              </div>

              <TotpSecret
                :secret="secret"
                testid="enroll-secret"
                copy-testid="enroll-copy-secret"
              />

              <form @submit.prevent="confirm">
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
                  class="enroll__failure"
                  :message="failure.summary"
                  :detail="failure.detail"
                  :request-id="failure.reference"
                />

                <IonButton
                  expand="block"
                  type="submit"
                  class="ion-margin-top"
                  data-testid="enroll-confirm"
                  :disabled="pending || code.length === 0"
                >
                  <IonSpinner v-if="pending" name="dots" />
                  <span v-else>Confirm</span>
                </IonButton>
              </form>
            </template>

            <template v-else>
              <PageHeader
                title="Save your recovery codes"
                subtitle="Each code signs you in once if you lose your authenticator."
              />

              <RecoveryCodes v-model="saved" :codes="codes" />

              <IonButton
                expand="block"
                class="ion-margin-top"
                data-testid="recovery-continue"
                :disabled="!saved"
                @click="finish"
              >
                Continue
              </IonButton>
            </template>
          </IonCardContent>
        </IonCard>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.enroll {
  display: flex;
  justify-content: center;
  padding: 8vh var(--ds-space-4) 0;
}

.enroll__card {
  max-width: 32rem;
  width: 100%;
}

.enroll__loading {
  align-items: center;
  color: var(--ds-text-muted);
  display: flex;
  gap: var(--ds-space-2);
}

.enroll__countdown {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-2);
}

.enroll__steps {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
  padding-left: var(--ds-space-5);
}

.enroll__qr {
  align-items: center;
  display: flex;
  justify-content: center;
  margin-bottom: var(--ds-space-3);
}

.enroll__qr img {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  padding: var(--ds-space-2);
}

.enroll__secret {
  align-items: center;
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  justify-content: space-between;
  margin-bottom: var(--ds-space-3);
  padding: var(--ds-space-3);
}

.enroll__secret-value {
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  letter-spacing: 0.08em;
  overflow-wrap: anywhere;
}

.enroll__copy {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.enroll__failure {
  margin-top: var(--ds-space-3);
}

.enroll__codes {
  display: grid;
  gap: var(--ds-space-2);
  grid-template-columns: repeat(2, minmax(0, 1fr));
  list-style: none;
  margin: 0 0 var(--ds-space-3);
  padding: 0;
}

.enroll__codes code {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  display: block;
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
  text-align: center;
}

.enroll__actions {
  display: flex;
  gap: var(--ds-space-2);
  margin-bottom: var(--ds-space-3);
}

.enroll__saved {
  --padding-start: 0;
}

.enroll__note {
  color: var(--ds-text-muted);
  display: block;
  font-size: var(--ds-font-size-sm);
  margin-top: var(--ds-space-2);
}
</style>
