<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonInput,
  IonItem,
  IonList,
  IonMenuButton,
  IonNote,
  IonPage,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onMounted, ref } from 'vue'

import {
  changePassword,
  confirmMfa,
  disableMfa,
  enrollMfa,
  fetchAccountStatus,
  listSessions,
  regenerateRecoveryCodes,
  revokeOtherSessions,
  type AccountEnrollment,
  type AccountSession,
} from '@/api/account'
import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { describeUserAgent } from '@/account/userAgent'
import ConfirmActionDialog from '@/admin/ConfirmActionDialog.vue'
import PasswordStrengthMeter from '@/components/auth/PasswordStrengthMeter.vue'
import RecoveryCodes from '@/components/auth/RecoveryCodes.vue'
import TotpQrCode from '@/components/auth/TotpQrCode.vue'
import TotpSecret from '@/components/auth/TotpSecret.vue'
import { DataTable, ErrorPanel, PageHeader, Panel } from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * The signed-in person's account page, at `/account`.
 *
 * Three panels over one bearer session: password, two-factor authentication and
 * live sessions. The page owns no authorization of its own — the server refuses
 * what it refuses, and the two role-shaped facts it does mirror are presentation:
 * an admin's factor is mandatory, so no "Turn off" is offered, and the root
 * account is password-only, which `/admin-auth/me` states with `is_root`.
 *
 * The account status comes from `fetchAccountStatus()` on mount, which reads
 * `is_root` and `mfa_enabled` from the caller's row; the page holds that state
 * itself. It never probes enrollment, because enrolling writes a pending secret:
 * `POST /admin-auth/mfa/enroll` is called only when "Set up" is pressed.
 */

const store = useSessionStore()

const toast = ref<string | null>(null)

// ---------------------------------------------------------------------------
// Password
// ---------------------------------------------------------------------------

const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')
const passwordPending = ref(false)
const passwordFailure = ref<FailureMessage | null>(null)

const passwordMismatch = computed(
  () => confirmPassword.value !== '' && confirmPassword.value !== newPassword.value,
)
const passwordReady = computed(
  () =>
    currentPassword.value.length > 0 &&
    newPassword.value.length > 0 &&
    confirmPassword.value === newPassword.value,
)

/**
 * `invalid_credentials` on this route means the CURRENT password was wrong, not
 * the sign-in pair, so it gets its own sentence rather than the sign-in one.
 */
function passwordFailureFor(error: unknown): FailureMessage {
  if (error instanceof ApiError && error.code === 'invalid_credentials') {
    return {
      summary: 'Your current password is not correct.',
      detail: error.message,
      reference: error.requestId,
    }
  }
  return describeFailure(error)
}

async function submitPassword(): Promise<void> {
  if (passwordPending.value || !passwordReady.value) return
  passwordPending.value = true
  passwordFailure.value = null
  try {
    await changePassword(currentPassword.value, newPassword.value)
    currentPassword.value = ''
    newPassword.value = ''
    confirmPassword.value = ''
    toast.value = 'Password changed; your other sessions were signed out'
    await refreshSessions()
  } catch (error) {
    passwordFailure.value = passwordFailureFor(error)
  } finally {
    passwordPending.value = false
  }
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessions = ref<AccountSession[]>([])
const sessionsLoading = ref(true)
const sessionsFailure = ref<FailureMessage | null>(null)
const sessionsError = ref<string | null>(null)

const revokeOpen = ref(false)
const revokePending = ref(false)

const sessionColumns: DataTableColumn[] = [
  { key: 'device', label: 'Device' },
  { key: 'ip', label: 'IP address' },
  { key: 'createdAt', label: 'Signed in' },
  { key: 'lastUsedAt', label: 'Last used' },
  { key: 'current', label: '' },
]

function formatWhen(value: string): string {
  if (value === '') return '—'
  const at = Date.parse(value)
  return Number.isNaN(at) ? value : new Date(at).toLocaleString()
}

async function loadSessions(): Promise<void> {
  sessionsLoading.value = true
  sessionsFailure.value = null
  try {
    sessions.value = await listSessions()
  } catch (error) {
    sessionsFailure.value = describeFailure(error)
  } finally {
    sessionsLoading.value = false
  }
}

/** Re-reads the list after a mutation without blanking it behind a spinner. */
async function refreshSessions(): Promise<void> {
  try {
    sessions.value = await listSessions()
  } catch (error) {
    sessionsError.value = describeFailure(error).summary
  }
}

async function confirmRevokeOthers(): Promise<void> {
  if (revokePending.value) return
  revokePending.value = true
  sessionsError.value = null
  try {
    const result = await revokeOtherSessions()
    revokeOpen.value = false
    toast.value = `Signed out ${result.revoked} other ${
      result.revoked === 1 ? 'session' : 'sessions'
    }`
    await refreshSessions()
  } catch (error) {
    revokeOpen.value = false
    sessionsError.value = describeFailure(error).summary
  } finally {
    revokePending.value = false
  }
}

// ---------------------------------------------------------------------------
// Two-factor authentication
// ---------------------------------------------------------------------------

type MfaStatus = 'loading' | 'enabled' | 'disabled' | 'root' | 'error'
type MfaPhase = 'idle' | 'setup' | 'code' | 'recovery'
type CodePurpose = 'regenerate' | 'disable'

const mfaStatus = ref<MfaStatus>('loading')
const mfaFailure = ref<FailureMessage | null>(null)
const mfaPhase = ref<MfaPhase>('idle')
const codePurpose = ref<CodePurpose | null>(null)
const enrollment = ref<AccountEnrollment | null>(null)
const mfaCode = ref('')
const mfaPending = ref(false)
const mfaCodeFailure = ref<FailureMessage | null>(null)
const newCodes = ref<string[]>([])
const codesSaved = ref(false)

const isAdmin = computed(() => store.roles.includes('admin'))

function openCode(purpose: CodePurpose): void {
  codePurpose.value = purpose
  mfaCode.value = ''
  mfaCodeFailure.value = null
  mfaPhase.value = 'code'
}

async function loadMfa(): Promise<void> {
  mfaStatus.value = 'loading'
  mfaFailure.value = null
  try {
    const status = await fetchAccountStatus()
    if (status.isRoot) {
      mfaStatus.value = 'root'
    } else {
      mfaStatus.value = status.mfaEnabled ? 'enabled' : 'disabled'
    }
  } catch (error) {
    mfaFailure.value = describeFailure(error)
    mfaStatus.value = 'error'
  }
}

function startSetup(): void {
  mfaCodeFailure.value = null
  if (enrollment.value !== null) {
    mfaPhase.value = 'setup'
    return
  }
  void beginSetup()
}

async function beginSetup(): Promise<void> {
  mfaCodeFailure.value = null
  try {
    enrollment.value = await enrollMfa()
    mfaStatus.value = 'disabled'
    mfaPhase.value = 'setup'
  } catch (error) {
    if (error instanceof ApiError && error.code === 'mfa_already_enabled') {
      mfaStatus.value = 'enabled'
      return
    }
    if (error instanceof ApiError && error.code === 'mfa_not_allowed') {
      mfaStatus.value = 'root'
      return
    }
    mfaFailure.value = describeFailure(error)
    mfaStatus.value = 'error'
  }
}

async function confirmSetup(): Promise<void> {
  if (mfaPending.value) return
  mfaPending.value = true
  mfaCodeFailure.value = null
  try {
    newCodes.value = await confirmMfa(mfaCode.value)
    codesSaved.value = false
    mfaStatus.value = 'enabled'
    mfaPhase.value = 'recovery'
  } catch (error) {
    mfaCodeFailure.value = describeFailure(error)
  } finally {
    mfaCode.value = ''
    mfaPending.value = false
  }
}

async function submitCode(): Promise<void> {
  if (mfaPending.value || codePurpose.value === null) return
  mfaPending.value = true
  mfaCodeFailure.value = null
  try {
    if (codePurpose.value === 'regenerate') {
      newCodes.value = await regenerateRecoveryCodes(mfaCode.value)
      codesSaved.value = false
      mfaPhase.value = 'recovery'
    } else {
      await disableMfa(mfaCode.value)
      mfaStatus.value = 'disabled'
      mfaPhase.value = 'idle'
      toast.value = 'Two-factor authentication turned off'
    }
  } catch (error) {
    mfaCodeFailure.value = describeFailure(error)
  } finally {
    mfaCode.value = ''
    mfaPending.value = false
  }
}

function finishRecovery(): void {
  mfaPhase.value = 'idle'
  codePurpose.value = null
  newCodes.value = []
  enrollment.value = null
}

onMounted(() => {
  void loadSessions()
  void loadMfa()
})
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="account-page">
        <PageHeader
          title="Account"
          subtitle="Your password, second factor and signed-in sessions."
        />

        <p v-if="toast !== null" class="account__toast" role="status" data-testid="account-toast">
          {{ toast }}
        </p>

        <Panel title="Password">
          <form data-testid="password-form" @submit.prevent="submitPassword">
            <IonList>
              <IonItem>
                <IonInput
                  v-model="currentPassword"
                  label="Current password"
                  label-placement="stacked"
                  type="password"
                  autocomplete="current-password"
                  required
                  :disabled="passwordPending"
                />
              </IonItem>
              <IonItem>
                <IonInput
                  v-model="newPassword"
                  label="New password"
                  label-placement="stacked"
                  type="password"
                  autocomplete="new-password"
                  required
                  :disabled="passwordPending"
                />
              </IonItem>
              <IonItem>
                <IonInput
                  v-model="confirmPassword"
                  label="Confirm password"
                  label-placement="stacked"
                  type="password"
                  autocomplete="new-password"
                  required
                  :disabled="passwordPending"
                />
              </IonItem>
            </IonList>

            <PasswordStrengthMeter :password="newPassword" :email="store.email ?? ''" />

            <IonNote v-if="passwordMismatch" color="danger" class="account__note">
              The two passwords do not match.
            </IonNote>

            <ErrorPanel
              v-if="passwordFailure !== null"
              class="account__failure"
              :message="passwordFailure.summary"
              :detail="passwordFailure.detail"
              :request-id="passwordFailure.reference"
            />

            <IonButton
              expand="block"
              type="submit"
              class="ion-margin-top"
              data-testid="password-submit"
              :disabled="passwordPending || !passwordReady"
            >
              <IonSpinner v-if="passwordPending" name="dots" />
              <span v-else>Change password</span>
            </IonButton>
          </form>
        </Panel>

        <Panel title="Two-factor authentication" class="account__panel">
          <div v-if="mfaStatus === 'loading'" class="account__busy" data-testid="mfa-loading">
            <IonSpinner name="dots" />
            <span>Checking your second factor…</span>
          </div>

          <ErrorPanel
            v-else-if="mfaStatus === 'error'"
            :message="mfaFailure?.summary ?? 'That did not work.'"
            :detail="mfaFailure?.detail"
            :request-id="mfaFailure?.reference ?? null"
            retry-label="Try again"
            @retry="loadMfa"
          />

          <p v-else-if="mfaStatus === 'root'" data-testid="mfa-root-note">
            The root account is password-only. It cannot use a second factor.
          </p>

          <template v-else-if="mfaPhase === 'setup' && enrollment !== null">
            <p class="account__lead">
              Add Otomo Admin to your authenticator app, scan the QR code (or enter the secret by
              hand), then enter the six-digit code it shows.
            </p>

            <div class="account__qr" data-testid="account-qr">
              <TotpQrCode :otpauth-url="enrollment.otpauthUrl" />
            </div>

            <TotpSecret
              :secret="enrollment.secret"
              testid="account-secret"
              copy-testid="account-copy-secret"
            />

            <form class="account__form" data-testid="mfa-setup-form" @submit.prevent="confirmSetup">
              <IonItem>
                <IonInput
                  v-model="mfaCode"
                  label="Code"
                  label-placement="stacked"
                  inputmode="numeric"
                  autocomplete="one-time-code"
                  required
                  :disabled="mfaPending"
                />
              </IonItem>

              <ErrorPanel
                v-if="mfaCodeFailure !== null"
                class="account__failure"
                :message="mfaCodeFailure.summary"
                :detail="mfaCodeFailure.detail"
                :request-id="mfaCodeFailure.reference"
              />

              <IonButton
                type="submit"
                class="ion-margin-top"
                data-testid="mfa-setup-confirm"
                :disabled="mfaPending || mfaCode.length === 0"
              >
                <IonSpinner v-if="mfaPending" name="dots" />
                <span v-else>Confirm</span>
              </IonButton>
            </form>
          </template>

          <template v-else-if="mfaPhase === 'code'">
            <p v-if="codePurpose === 'disable'" data-testid="mfa-disable-warning">
              Turning off two-factor authentication removes your authenticator factor and your
              recovery codes. You will sign in with your password alone.
            </p>
            <p v-else class="account__lead">
              Enter a current code from your authenticator to replace your recovery codes.
            </p>

            <form class="account__form" data-testid="mfa-code-form" @submit.prevent="submitCode">
              <IonItem>
                <IonInput
                  v-model="mfaCode"
                  label="Code"
                  label-placement="stacked"
                  inputmode="numeric"
                  autocomplete="one-time-code"
                  required
                  :disabled="mfaPending"
                />
              </IonItem>

              <ErrorPanel
                v-if="mfaCodeFailure !== null"
                class="account__failure"
                :message="mfaCodeFailure.summary"
                :detail="mfaCodeFailure.detail"
                :request-id="mfaCodeFailure.reference"
              />

              <IonButton
                type="submit"
                class="ion-margin-top"
                data-testid="mfa-code-submit"
                :disabled="mfaPending || mfaCode.length === 0"
              >
                <IonSpinner v-if="mfaPending" name="dots" />
                <span v-else>{{ codePurpose === 'disable' ? 'Turn off' : 'Regenerate' }}</span>
              </IonButton>
              <IonButton
                fill="clear"
                class="ion-margin-top"
                data-testid="mfa-code-cancel"
                @click="mfaPhase = 'idle'"
              >
                Cancel
              </IonButton>
            </form>
          </template>

          <template v-else-if="mfaPhase === 'recovery'">
            <p class="account__lead">
              Save these new recovery codes. Each one signs you in once; the previous set no longer
              works.
            </p>

            <RecoveryCodes v-model="codesSaved" :codes="newCodes" />

            <IonButton
              expand="block"
              class="ion-margin-top"
              data-testid="mfa-recovery-continue"
              :disabled="!codesSaved"
              @click="finishRecovery"
            >
              Continue
            </IonButton>
          </template>

          <template v-else>
            <p data-testid="mfa-status">
              {{
                mfaStatus === 'enabled'
                  ? 'Two-factor authentication is enabled.'
                  : 'Two-factor authentication is not enabled.'
              }}
            </p>
            <p v-if="isAdmin" class="account__note" data-testid="mfa-admin-note">
              Two-factor authentication is required for admin accounts.
            </p>

            <div class="account__actions">
              <IonButton
                v-if="mfaStatus === 'disabled'"
                data-testid="mfa-setup"
                @click="startSetup"
              >
                Set up
              </IonButton>
              <IonButton
                v-if="mfaStatus === 'enabled'"
                fill="outline"
                data-testid="mfa-regenerate"
                @click="openCode('regenerate')"
              >
                Regenerate recovery codes
              </IonButton>
              <IonButton
                v-if="mfaStatus === 'enabled' && !isAdmin"
                fill="outline"
                color="danger"
                data-testid="mfa-disable"
                @click="openCode('disable')"
              >
                Turn off
              </IonButton>
            </div>
          </template>
        </Panel>

        <Panel title="Sessions" :padding="false" class="account__panel">
          <div class="account__sessions">
            <div class="account__session-bar">
              <IonButton fill="outline" data-testid="revoke-others" @click="revokeOpen = true">
                Sign out other sessions
              </IonButton>
            </div>

            <p
              v-if="sessionsError !== null"
              class="account__error"
              role="alert"
              data-testid="sessions-error"
            >
              {{ sessionsError }}
            </p>

            <ErrorPanel
              v-if="sessionsFailure !== null"
              class="account__failure"
              :message="sessionsFailure.summary"
              :detail="sessionsFailure.detail"
              :request-id="sessionsFailure.reference"
              retry-label="Try again"
              @retry="loadSessions"
            />
            <DataTable
              v-else
              :columns="sessionColumns"
              :rows="sessions"
              row-key="id"
              :loading="sessionsLoading"
            >
              <template #cell-device="{ row }">{{ describeUserAgent(row.userAgent) }}</template>
              <template #cell-createdAt="{ row }">{{ formatWhen(row.createdAt) }}</template>
              <template #cell-lastUsedAt="{ row }">{{ formatWhen(row.lastUsedAt) }}</template>
              <template #cell-current="{ row }">
                <span v-if="row.current" class="account__current" data-testid="session-current">
                  This device
                </span>
              </template>
            </DataTable>
          </div>
        </Panel>
      </div>
    </IonContent>

    <ConfirmActionDialog
      v-if="revokeOpen"
      :is-open="true"
      title="Sign out other sessions"
      message="Sign out every session except this one? Anything signed in elsewhere stops immediately."
      confirm-label="Sign out others"
      danger
      :pending="revokePending"
      @confirm="confirmRevokeOthers"
      @cancel="revokeOpen = false"
    />
  </IonPage>
</template>

<style scoped>
.account__toast {
  background: var(--ds-ok-soft);
  border: 1px solid var(--ds-ok);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-ok);
  font-weight: 600;
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.account__panel {
  margin-top: var(--ds-space-5);
}

.account__busy {
  align-items: center;
  color: var(--ds-text-muted);
  display: flex;
  gap: var(--ds-space-2);
}

.account__lead {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.account__note {
  color: var(--ds-text-muted);
  display: block;
  font-size: var(--ds-font-size-sm);
  margin-top: var(--ds-space-2);
}

.account__qr {
  align-items: center;
  display: flex;
  justify-content: center;
  margin-bottom: var(--ds-space-3);
}

.account__form {
  margin-top: var(--ds-space-3);
}

.account__failure {
  margin-top: var(--ds-space-3);
}

.account__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-3);
}

.account__sessions {
  padding: var(--ds-space-4);
}

.account__session-bar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: var(--ds-space-3);
}

.account__error {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-danger);
  margin-bottom: var(--ds-space-3);
  padding: var(--ds-space-3);
}

.account__current {
  background: var(--ds-ok-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-ok);
  display: inline-block;
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  padding: var(--ds-space-1) var(--ds-space-2);
  white-space: nowrap;
}
</style>
