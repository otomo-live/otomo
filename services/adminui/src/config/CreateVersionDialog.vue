<script setup lang="ts">
import {
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonModal,
  IonSpinner,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import {
  createVersion,
  getDiff,
  getDraft,
  isNoChanges,
  isStaleRevision,
  listVersions,
  type DraftSnapshot,
  type VersionDiff,
} from '@/api/config'
import { ApiError } from '@/api/errors'
import { describeFailure } from '@/api/messages'
import ConflictDialog from '@/config/ConflictDialog.vue'
import DiffView from '@/config/DiffView.vue'
import type { DocumentChange } from '@/config/diff'

/**
 * The create-version dialog.
 *
 * A version is cut from the SERVER draft, not from this editor's in-memory copy,
 * so the dialog says which revision it is about to send and previews the diff
 * between that draft and the latest published version. The message is required;
 * empty or whitespace-only disables the button and shows the reason inline.
 *
 * Every refusal the endpoint has a meaning for is handled here:
 *
 *  - `409 stale_revision` — someone changed the draft after this dialog read its
 *    revision. It is the same two-way question as a save conflict, so the
 *    existing `ConflictDialog` asks it: reload the newer draft, or version the
 *    newer draft rather than the one that was reviewed.
 *  - `409 no_changes` — the draft already IS the latest version; shown inline.
 *  - `400 validation_failed` — the draft does not satisfy its schema; shown
 *    verbatim because the place it names is what has to change.
 */

const props = defineProps<{
  isOpen: boolean
  namespace: string
  /** The server revision this dialog will send. */
  revision: number
  /** The unsaved changes the editor holds, shown if a conflict is raised. */
  localChanges: DocumentChange[]
}>()

const emit = defineEmits<{
  created: [version: number]
  dismiss: []
  reloadDraft: []
}>()

const message = ref('')
const busy = ref(false)
const loadingContext = ref(false)
const error = ref<string | null>(null)
const diff = ref<VersionDiff | null>(null)
const conflict = ref<DraftSnapshot | null>(null)
const revisionToSend = ref(props.revision)

// The draft equals the latest version: Config would answer 409 no_changes, so say
// so up front instead of letting the person type a message for nothing.
const nothingToVersion = computed(() => diff.value !== null && diff.value.changes.length === 0)
const ready = computed(
  () =>
    message.value.trim() !== '' &&
    !busy.value &&
    !loadingContext.value &&
    conflict.value === null &&
    !nothingToVersion.value,
)
const messageEmpty = computed(() => message.value.trim() === '')

watch(
  () => props.revision,
  (value) => {
    revisionToSend.value = value
  },
)

watch(
  () => [props.isOpen, props.namespace] as const,
  async ([open]) => {
    if (!open) return
    await loadContext()
  },
  { immediate: true },
)

/**
 * The draft-vs-latest preview. It is read fresh on every open, because the draft
 * is the server's and someone may have changed it since the last time this
 * dialog was shown.
 */
async function loadContext(): Promise<void> {
  loadingContext.value = true
  error.value = null
  conflict.value = null
  revisionToSend.value = props.revision
  try {
    const page = await listVersions(props.namespace, { limit: 1 })
    const latest = page.versions[0]?.version ?? null
    diff.value = latest === null ? null : await getDiff(props.namespace, String(latest), 'draft')
  } catch (caught) {
    error.value = describeFailure(caught).summary
    diff.value = null
  } finally {
    loadingContext.value = false
  }
}

async function submit(): Promise<void> {
  if (!ready.value) return
  busy.value = true
  error.value = null
  try {
    const created = await createVersion(props.namespace, {
      message: message.value.trim(),
      revision: revisionToSend.value,
    })
    message.value = ''
    emit('created', created.version)
  } catch (caught) {
    if (isStaleRevision(caught)) {
      try {
        conflict.value = await getDraft(props.namespace)
      } catch (readError) {
        error.value = describeFailure(readError).summary
      }
    } else if (isNoChanges(caught)) {
      error.value = `Nothing to version: ${
        caught instanceof ApiError ? caught.message : 'the draft is unchanged'
      }`
    } else if (caught instanceof ApiError && caught.code === 'validation_failed') {
      error.value = caught.message
    } else {
      error.value = describeFailure(caught).summary
    }
  } finally {
    busy.value = false
  }
}

/** Reloading the newer draft abandons this attempt; the editor reloads. */
function onConflictReload(): void {
  conflict.value = null
  emit('reloadDraft')
}

/**
 * "Keep mine" here means "version the newer draft": the server draft is what a
 * version is cut from either way, so the honest forward step is to adopt the
 * revision the dialog reported and try again against it.
 */
async function onConflictKeep(): Promise<void> {
  const fresh = conflict.value
  if (fresh === null) return
  revisionToSend.value = fresh.revision
  conflict.value = null
  await submit()
}
</script>

<template>
  <ConflictDialog
    v-if="conflict !== null"
    :is-open="true"
    :local-revision="revisionToSend"
    :server-revision="conflict.revision"
    :server-updated-by="conflict.updatedBy"
    :server-updated-at="conflict.updatedAt"
    :changes="props.localChanges"
    :pending="busy"
    title="Someone else changed the draft"
    reload-label="Reload the newer draft"
    keep-label="Version the newer draft"
    @dismiss="conflict = null"
    @reload="onConflictReload"
    @overwrite="onConflictKeep"
  />

  <IonModal v-else :is-open="props.isOpen" @did-dismiss="emit('dismiss')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>Create version</IonTitle>
        <IonButtons slot="end">
          <IonButton :disabled="busy" @click="emit('dismiss')">Cancel</IonButton>
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent class="ion-padding">
      <p class="create__meta" data-testid="create-version-revision">
        Cutting from draft revision <strong>{{ revisionToSend }}</strong> of
        <code>{{ props.namespace }}</code
        >.
      </p>

      <label class="create__field">
        <span class="create__label">Message</span>
        <textarea
          v-model="message"
          class="create__message"
          data-testid="create-version-message"
          rows="3"
          placeholder="What changed in this version?"
        />
      </label>
      <p v-if="messageEmpty" class="create__hint" data-testid="create-version-message-hint">
        A message is required.
      </p>

      <p
        v-if="error !== null"
        class="create__error"
        role="alert"
        data-testid="create-version-error"
      >
        {{ error }}
      </p>

      <h2 class="create__heading">Draft vs latest version</h2>
      <p v-if="loadingContext" class="create__hint">Loading the preview...</p>
      <template v-else>
        <DiffView
          v-if="diff !== null"
          :changes="diff.changes"
          :from-ref="diff.from.ref"
          :to-ref="diff.to.ref"
          :from-document="diff.from.document"
          :to-document="diff.to.document"
          compact
        />
        <p v-if="nothingToVersion" class="create__hint" data-testid="create-version-nothing">
          Nothing to version: the draft is identical to {{ diff?.from.ref }}. Save a change to the
          draft first.
        </p>
        <p v-if="diff === null" class="create__hint" data-testid="create-version-first">
          This will be the first version of the namespace.
        </p>
      </template>

      <div class="create__actions">
        <button
          type="button"
          class="create__submit"
          :disabled="!ready"
          data-testid="create-version-submit"
          @click="submit"
        >
          <IonSpinner v-if="busy" name="dots" />
          <span v-else>Create version</span>
        </button>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.create__meta {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.create__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.create__label {
  color: var(--ds-text);
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
}

.create__message {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
  resize: vertical;
  width: 100%;
}

.create__message:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.create__submit {
  align-items: center;
  background: var(--ds-accent);
  border: 1px solid var(--ds-accent);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-accent-contrast);
  cursor: pointer;
  display: inline-flex;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  justify-content: center;
  min-height: 2.25rem;
  padding: var(--ds-space-2) var(--ds-space-4);
  width: 100%;
}

.create__submit:disabled {
  cursor: default;
  opacity: 0.6;
}

.create__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0 0;
}

.create__error {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
}

.create__heading {
  color: var(--ds-text);
  font-size: var(--ds-font-size-md);
  margin: var(--ds-space-5) 0 var(--ds-space-2);
}

.create__actions {
  margin-top: var(--ds-space-5);
}
</style>
