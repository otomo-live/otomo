<script setup lang="ts">
import {
  IonBackButton,
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonMenuButton,
  IonPage,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { onBeforeRouteLeave, useRoute } from 'vue-router'

import { getSchema, isStaleSchema, putSchema, type SchemaSnapshot } from '@/api/config'
import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { hasRoleAtLeast } from '@/auth/roles'
import { Breadcrumbs, PageHeader, Panel } from '@/components/ui'
import RoleGate from '@/components/RoleGate.vue'
import RawJsonEditor from '@/config/RawJsonEditor.vue'
import { serialise } from '@/config/document'
import { parseSchemaText } from '@/config/schemaDocument'
import { useSessionStore } from '@/stores/session'

/**
 * One namespace's schema, read-only for everyone and editable for an admin.
 *
 * Replacing a schema writes an immutable new `schema_version` (CFG-B2), so the
 * page treats the loaded snapshot as fixed: pressing Edit does not lock
 * anything on the server, and there is no version precondition on the PUT. The
 * lost-update guard is therefore done by hand, right before the write: re-read
 * the schema, and if its version moved since this page loaded, stop and make the
 * admin choose. The alternative — writing and discovering — is not available,
 * because the server would accept the write and silently discard whoever saved
 * in between.
 *
 * The editor's own text is the document. A syntax error is shown with its line
 * and Save does nothing; a schema Config refuses (400) keeps its message under
 * the editor and keeps the text, because the fix is in the text.
 */

const route = useRoute()
const session = useSessionStore()

const namespace = computed(() => String(route.params.name ?? ''))

const loading = ref(true)
const failure = ref<FailureMessage | null>(null)

const schema = ref<SchemaSnapshot | null>(null)
const text = ref('')
/** What the server last gave us: the baseline unsaved changes are against. */
const baseline = ref('')

const editing = ref(false)
const saving = ref(false)
const parseError = ref<{ message: string; line: number | null } | null>(null)
const serverError = ref<string | null>(null)
const toast = ref<string | null>(null)
/** A newer schema that arrived from the guard, waiting for Reload or Save anyway. */
const conflict = ref<SchemaSnapshot | null>(null)

const isAdmin = computed(
  () => session.status === 'authenticated' && hasRoleAtLeast(session.roles, 'admin'),
)
const dirty = computed(() => text.value !== baseline.value)
const nextVersion = computed(() => (schema.value?.schemaVersion ?? 0) + 1)

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const snapshot = await getSchema(namespace.value)
    schema.value = snapshot
    text.value = serialise(snapshot.schema)
    baseline.value = text.value
    editing.value = false
    parseError.value = null
    serverError.value = null
    toast.value = null
    conflict.value = null
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

function onInput(value: string): void {
  text.value = value
  parseError.value = null
  toast.value = null
}

function beginEdit(): void {
  if (!isAdmin.value) return
  serverError.value = null
  toast.value = null
  editing.value = true
}

function cancelEdit(): void {
  text.value = baseline.value
  parseError.value = null
  serverError.value = null
  editing.value = false
}

function readDocument(): unknown | null {
  const parsed = parseSchemaText(text.value)
  if (parsed.kind === 'error') {
    parseError.value = { message: parsed.message, line: parsed.line }
    return null
  }
  parseError.value = null
  return parsed.document
}

/**
 * The write itself. Separate so both save paths end here. `expected` is the schema
 * version the edit is based on; Config compares it under its row lock and answers
 * 409 stale_schema when someone saved in between, which opens the conflict dialog
 * with the version that won.
 */
async function performSave(document: unknown, expected: number): Promise<void> {
  try {
    const saved = await putSchema(namespace.value, document, expected)
    schema.value = saved
    baseline.value = text.value
    editing.value = false
    serverError.value = null
    toast.value = `Schema v${saved.schemaVersion} saved`
  } catch (error) {
    if (isStaleSchema(error)) {
      try {
        conflict.value = await getSchema(namespace.value)
      } catch (reloadError) {
        serverError.value = describeFailure(reloadError).summary
      }
      return
    }
    // A 400 names a place in the schema and is worth showing verbatim. Anything
    // else (403, 500, offline) is not about the text, so it gets the mapped
    // sentence. The text is never touched: the admin's work stays on screen.
    serverError.value =
      error instanceof ApiError && error.code === 'validation_failed'
        ? error.message
        : describeFailure(error).summary
  }
}

async function save(): Promise<void> {
  if (saving.value) return
  serverError.value = null
  const document = readDocument()
  if (document === null) return

  saving.value = true
  try {
    // The lost-update guard is Config's own (If-Match): the write names
    // the version this page loaded or last saved, and a newer one comes back as a
    // conflict instead of being overwritten.
    await performSave(document, schema.value?.schemaVersion ?? 1)
  } catch (error) {
    serverError.value = describeFailure(error).summary
  } finally {
    saving.value = false
  }
}

function reload(): void {
  const fresh = conflict.value
  if (fresh === null) return
  schema.value = fresh
  text.value = serialise(fresh.schema)
  baseline.value = text.value
  conflict.value = null
  editing.value = false
  parseError.value = null
  serverError.value = null
}

async function saveAnyway(): Promise<void> {
  const fresh = conflict.value
  if (fresh === null) return
  const document = readDocument()
  if (document === null) {
    conflict.value = null
    return
  }
  conflict.value = null
  saving.value = true
  try {
    // "Save anyway" is a deliberate overwrite of the version the dialog showed; if
    // yet another save lands meanwhile, Config refuses again and the dialog returns.
    await performSave(document, fresh.schemaVersion)
  } finally {
    saving.value = false
  }
}

/**
 * The two guards out of a page with unsaved edits: the router one for in-app
 * navigation and `beforeunload` for a tab close or reload, which the router
 * never sees.
 */
function onBeforeUnload(event: BeforeUnloadEvent): void {
  if (!dirty.value) return
  event.preventDefault()
  // Older browsers require returnValue to be set for the prompt to appear.
  event.returnValue = ''
}

onBeforeRouteLeave(() => {
  if (!dirty.value || saving.value) return true
  return window.confirm('You have unsaved schema changes. Leave without saving?')
})

onMounted(() => {
  window.addEventListener('beforeunload', onBeforeUnload)
  void load()
})

onBeforeUnmount(() => window.removeEventListener('beforeunload', onBeforeUnload))
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonButtons slot="start">
          <IonMenuButton />
          <IonBackButton default-href="/config" />
        </IonButtons>
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page">
        <PageHeader :title="`${namespace} schema`">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <RoleGate role="admin">
              <template v-if="!editing">
                <IonButton
                  data-testid="schema-edit"
                  :disabled="loading || failure !== null"
                  @click="beginEdit"
                >
                  Edit
                </IonButton>
              </template>
              <template v-else>
                <IonButton data-testid="schema-save" :disabled="!dirty || saving" @click="save">
                  <IonSpinner v-if="saving" name="dots" />
                  <span v-else>Save as version {{ nextVersion }}</span>
                </IonButton>
                <IonButton fill="clear" color="medium" :disabled="saving" @click="cancelEdit">
                  Cancel
                </IonButton>
              </template>
            </RoleGate>
          </template>
        </PageHeader>

        <div v-if="loading" class="schema__busy">
          <IonSpinner name="dots" />
          <p>Loading {{ namespace }} schema...</p>
        </div>

        <p v-else-if="failure !== null" class="schema__error" role="alert">
          {{ failure.summary }} {{ failure.detail }}
        </p>

        <template v-else>
          <p class="schema__meta">
            <span class="schema__namespace">{{ namespace }}</span>
            &middot; schema v{{ schema?.schemaVersion }}
            <span v-if="schema !== null && schema.createdBy !== ''">
              &middot; saved by {{ schema.createdBy }} at {{ schema.createdAt }}
            </span>
          </p>

          <RawJsonEditor
            :model-value="text"
            :readonly="!editing"
            :aria-label="`${namespace} schema as JSON`"
            @update:model-value="onInput"
          />

          <p
            v-if="parseError !== null"
            class="schema__error"
            role="alert"
            data-testid="schema-parse-error"
          >
            <span v-if="parseError.line !== null">Line {{ parseError.line }}: </span>
            {{ parseError.message }}
          </p>

          <p
            v-if="serverError !== null"
            class="schema__error"
            role="alert"
            data-testid="schema-server-error"
          >
            {{ serverError }}
          </p>

          <p v-if="toast !== null" class="schema__toast" role="status" data-testid="schema-toast">
            {{ toast }}
          </p>

          <Panel
            v-if="conflict !== null"
            title="Someone else saved this schema"
            class="schema__block"
          >
            <p data-testid="schema-conflict-message">
              Someone saved schema v{{ conflict.schemaVersion }} while you were editing.
            </p>
            <div class="schema__actions">
              <IonButton data-testid="schema-conflict-reload" @click="reload">Reload</IonButton>
              <IonButton
                color="danger"
                fill="outline"
                :disabled="saving"
                data-testid="schema-conflict-save-anyway"
                @click="saveAnyway"
              >
                Save anyway
              </IonButton>
            </div>
          </Panel>
        </template>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.schema__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.schema__meta {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.schema__namespace {
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
}

.schema__error {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
}

.schema__toast {
  color: var(--ds-ok);
  font-size: var(--ds-font-size-sm);
}

.schema__block {
  margin-top: var(--ds-space-5);
}

.schema__actions {
  display: flex;
  gap: var(--ds-space-2);
}
</style>
