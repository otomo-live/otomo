<script setup lang="ts">
import {
  IonBackButton,
  IonButton,
  IonButtons,
  IonContent,
  IonHeader,
  IonMenuButton,
  IonPage,
  IonSegment,
  IonSegmentButton,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import {
  getDiff,
  getDraft,
  getSchema,
  isStaleRevision,
  saveDraft,
  validateDocument,
  type DraftSnapshot,
  type SchemaSnapshot,
  type ValidationIssue,
  type VersionDiff,
} from '@/api/config'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { hasRoleAtLeast } from '@/auth/roles'
import {
  Breadcrumbs,
  CopyableId,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Panel,
  Toolbar,
} from '@/components/ui'
import RoleGate from '@/components/RoleGate.vue'
import ConflictDialog from '@/config/ConflictDialog.vue'
import CreateVersionDialog from '@/config/CreateVersionDialog.vue'
import DiffView from '@/config/DiffView.vue'
import RawJsonEditor from '@/config/RawJsonEditor.vue'
import SchemaField from '@/config/SchemaField.vue'
import VersionsTab from '@/config/VersionsTab.vue'
import { diffDocuments, formatPointer, type DocumentChange } from '@/config/diff'
import { clone, fieldOf, parseRaw, serialise, unclaimedIssues, withField } from '@/config/document'
import { buildForm, loadEnvelope, type FormModel } from '@/config/presentation'
import { validateLocally } from '@/config/validation'
import { useSessionStore } from '@/stores/session'

/**
 * The schema-driven editor for one namespace.
 *
 * Everything on screen is derived from documents the SERVER holds: the
 * namespace's JSON Schema, the draft, and the immutable versions. Nothing here
 * knows what a namespace contains. A game's table is arbitrary JSON with its own
 * schema, so the form is generated from the schema, the labels and grouping come
 * from optional presentation metadata the project authored, and whatever the form
 * model cannot lay out falls back to a raw JSON editor for that field alone.
 *
 * The reconciliation rule, which is the whole design of the two editing modes:
 * **the parsed document is the single source of truth, always.** The form writes
 * into it, the raw tab parses into it on every keystroke, and a save always
 * stores the document in hand. There is never a text buffer and a form model
 * that could disagree about what is about to be saved.
 *
 * Role gating is presentation (WEB-5). A viewer may open the route and read the
 * form, the JSON and the version history, but the editor renders no mutating
 * control for them and the gateway refuses the request regardless.
 *
 * Saving is a compare-and-set (CFG-B3): the revision the draft was read at goes
 * with the document, Config updates only where it still matches, and a 409 is
 * the two-way choice in ConflictDialog. Cutting a version is a third mutation,
 * cut from the server draft at the same revision, and it has its own dialog and
 * its own conflict path.
 */

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const namespace = computed(() => String(route.params.namespace ?? ''))

const loading = ref(true)
const failure = ref<FailureMessage | null>(null)

const schema = ref<SchemaSnapshot | null>(null)
const form = ref<FormModel | null>(null)

/** The document in hand: what the form and the raw tab both write into. */
const document = ref<unknown>({})
/** What the server last gave us. The baseline the unsaved changes are against. */
const baseline = ref<unknown>({})

const revision = ref(0)
const baseVersion = ref(0)
const updatedBy = ref('')
const updatedAt = ref('')

type EditorTab = 'form' | 'raw' | 'versions'
const tab = ref<EditorTab>(initialTab())
const rawText = ref('')
/** Non-null while the raw text is not a document: the reason, in a sentence. */
const rawProblem = ref<string | null>(null)
const showChanges = ref(false)

/** Versions tab: the two refs being compared, in selection order. */
const from = ref<string | null>(initialParam('from'))
const to = ref<string | null>(initialParam('to'))
const diff = ref<VersionDiff | null>(null)
const diffFailure = ref<FailureMessage | null>(null)
const diffLoading = ref(false)
/** Bumped after a version is created so the history list reloads. */
const versionsRefresh = ref(0)

const createOpen = ref(false)
const versionToast = ref<string | null>(null)

/** The local validator's opinion, which is a hint. Config's is the authority, and it is below. */
const localIssues = ref<ValidationIssue[]>([])
/** What Config's validator said on the last save attempt. It wins over the local check's. */
const serverIssues = ref<ValidationIssue[] | null>(null)
const schemaUnavailable = ref<string | null>(null)

const saving = ref(false)
const savedAt = ref<string | null>(null)
const saveFailure = ref<FailureMessage | null>(null)
const conflict = ref<DraftSnapshot | null>(null)

const canEdit = computed(
  () => session.status === 'authenticated' && hasRoleAtLeast(session.roles, 'live_ops'),
)

const issues = computed(() => serverIssues.value ?? localIssues.value)
const changes = computed<DocumentChange[]>(() => diffDocuments(baseline.value, document.value))
const dirty = computed(() => changes.value.length > 0)

const fieldPointers = computed(() =>
  (form.value?.sections ?? []).flatMap((section) =>
    section.fields.map((field) => `/${field.name}`),
  ),
)
const unclaimed = computed(() => unclaimedIssues(issues.value, fieldPointers.value))

const canSave = computed(
  () =>
    canEdit.value && !loading.value && !saving.value && dirty.value && rawProblem.value === null,
)

const comparing = computed(() => from.value !== null && to.value !== null)

function initialTab(): EditorTab {
  if (route.query.tab === 'versions') return 'versions'
  if (route.query.tab === 'raw') return 'raw'
  return 'form'
}

function initialParam(name: 'from' | 'to'): string | null {
  const value = route.query[name]
  return typeof value === 'string' && value !== '' ? value : null
}

/**
 * Revalidates the document in hand.
 *
 * Both validators run against the SAME document, which is what keeps a locally
 * valid form and a stored document the same thing. A schema the local validator
 * cannot use is reported as unavailable rather than as valid: an editor that
 * says "no
 * problems" because it never managed to check is worse than one that admits it
 * does not know.
 */
function revalidate(): void {
  serverIssues.value = null
  if (schema.value === null) return

  const result = validateLocally(schema.value.schema, document.value, cacheKey())
  if (result.kind === 'valid') {
    localIssues.value = []
    schemaUnavailable.value = null
    return
  }
  if (result.kind === 'invalid') {
    localIssues.value = result.errors
    schemaUnavailable.value = null
    return
  }
  localIssues.value = []
  schemaUnavailable.value = result.reason
}

function cacheKey(): string {
  return `${namespace.value}@${schema.value?.schemaVersion ?? 0}`
}

function adopt(document_: unknown): void {
  document.value = document_
  revalidate()
}

function setField(name: string, value: unknown): void {
  if (!canEdit.value) return
  adopt(withField(document.value, name, value))
}

function onRawInput(text: string): void {
  if (!canEdit.value) return
  rawText.value = text
  const parsed = parseRaw(text)
  if (parsed.kind === 'document') {
    // Live, so the unsaved-changes list and the save button tell the truth about
    // what is in the text editor rather than about what it last parsed to.
    rawProblem.value = null
    adopt(parsed.document)
    return
  }
  rawProblem.value = parsed.message
}

function toRaw(): void {
  // Always re-serialised from the document. The form is the authority while you
  // are in the form, so the raw view shows the document as it stands, with the
  // indentation a document would be stored with.
  rawText.value = serialise(document.value)
  rawProblem.value = null
  tab.value = 'raw'
}

function toForm(): void {
  if (rawProblem.value !== null) return
  tab.value = 'form'
}

/** The segment's `update:modelValue` carries `string | number | undefined`. */
function onTabChange(value: string | number | undefined): void {
  if (value === 'raw') toRaw()
  else if (value === 'form') toForm()
  else if (value === 'versions') tab.value = 'versions'
}

function revert(): void {
  if (!canEdit.value) return
  adopt(clone(baseline.value))
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    // In parallel: the editor needs both, and neither depends on the other. The
    // envelope is a third read that is allowed to fail on its own (a project
    // that has authored no presentation metadata has no such namespace at all),
    // so it is not in this Promise.all.
    const [schemaSnapshot, draft] = await Promise.all([
      getSchema(namespace.value),
      getDraft(namespace.value),
    ])
    const envelope = await loadEnvelope(namespace.value)

    schema.value = schemaSnapshot
    form.value = buildForm(schemaSnapshot.schema, envelope)

    baseline.value = clone(draft.document)
    document.value = clone(draft.document)
    revision.value = draft.revision
    baseVersion.value = draft.baseVersion
    updatedBy.value = draft.updatedBy
    updatedAt.value = draft.updatedAt

    rawProblem.value = null
    if (tab.value === 'raw') rawText.value = serialise(document.value)
    savedAt.value = null
    saveFailure.value = null
    versionToast.value = null
    conflict.value = null
    revalidate()
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

async function save(): Promise<void> {
  if (saving.value || !canEdit.value) return
  saving.value = true
  saveFailure.value = null

  try {
    // Config's validator first, because it is the authority and because a
    // document that fails it would be stored as a draft that cannot become a
    // version (CFG-B4). The local validator already warned; this decides.
    const verdict = await validateDocument(namespace.value, document.value)
    if (!verdict.valid) {
      serverIssues.value = verdict.errors
      return
    }
    serverIssues.value = null

    const saved = await saveDraft(namespace.value, document.value, revision.value)
    revision.value = saved.revision
    savedAt.value = saved.updatedAt
    baseline.value = clone(document.value)
  } catch (error) {
    if (isStaleRevision(error)) {
      // Read the newer draft so the dialog can say what it moved to, and offer
      // the choice with the real numbers rather than the message's.
      try {
        conflict.value = await getDraft(namespace.value)
      } catch (readError) {
        saveFailure.value = describeFailure(readError)
      }
      return
    }
    saveFailure.value = describeFailure(error)
  } finally {
    saving.value = false
  }
}

function reloadFromServer(): void {
  const fresh = conflict.value
  if (fresh === null) return
  conflict.value = null
  baseline.value = clone(fresh.document)
  document.value = clone(fresh.document)
  revision.value = fresh.revision
  baseVersion.value = fresh.baseVersion
  updatedBy.value = fresh.updatedBy
  updatedAt.value = fresh.updatedAt
  rawProblem.value = null
  serverIssues.value = null
  if (tab.value === 'raw') rawText.value = serialise(document.value)
  revalidate()
}

/**
 * Take the newer revision as ours and save over it.
 *
 * The revision the dialog reported is adopted first, so the next save is a
 * compare-and-set against the draft as it stands rather than against the one we
 * were refused at. If someone saves again in between, that save 409s too and the
 * dialog comes back with the newer numbers, which is the honest outcome: it is
 * still a race, and it is still the user's call.
 */
async function overwrite(): Promise<void> {
  const fresh = conflict.value
  if (fresh === null) return
  conflict.value = null
  revision.value = fresh.revision
  baseline.value = clone(fresh.document)
  await save()
}

/** The create-version button: local edits have to be saved before versioning. */
async function openCreate(): Promise<void> {
  if (!canEdit.value) return
  if (dirty.value) {
    if (!window.confirm('Save your draft before creating a version?')) return
    await save()
    if (dirty.value) return
  }
  versionToast.value = null
  createOpen.value = true
}

/** Adopt the draft the server now holds, usually right after a version was cut. */
async function refreshDraft(): Promise<void> {
  try {
    const fresh = await getDraft(namespace.value)
    baseline.value = clone(fresh.document)
    document.value = clone(fresh.document)
    revision.value = fresh.revision
    baseVersion.value = fresh.baseVersion
    updatedBy.value = fresh.updatedBy
    updatedAt.value = fresh.updatedAt
    serverIssues.value = null
    saveFailure.value = null
    if (tab.value === 'raw') rawText.value = serialise(document.value)
    revalidate()
  } catch (error) {
    saveFailure.value = describeFailure(error)
  }
}

async function onVersionCreated(version: number): Promise<void> {
  createOpen.value = false
  versionToast.value = `Version ${version} created`
  versionsRefresh.value += 1
  await refreshDraft()
}

async function onReloadDraftFromCreate(): Promise<void> {
  createOpen.value = false
  await load()
}

/**
 * A row click in the history moves a two-ref selection.
 *
 * The pair is at most two, in insertion order, and clicking an already-selected
 * ref removes it. Selecting a third keeps the two most recent, which is what a
 * reader expects from "compare these": the newest click is the `to` side.
 */
function onSelectVersion(reference: string): void {
  const chosen = [from.value, to.value].filter((value): value is string => value !== null)
  const existing = chosen.indexOf(reference)
  if (existing >= 0) chosen.splice(existing, 1)
  else chosen.push(reference)
  const trimmed = chosen.slice(-2)
  from.value = trimmed[0] ?? null
  to.value = trimmed[1] ?? null
}

async function fetchDiff(): Promise<void> {
  if (tab.value !== 'versions' || namespace.value === '' || !comparing.value) {
    diff.value = null
    diffFailure.value = null
    return
  }
  diffLoading.value = true
  diffFailure.value = null
  try {
    diff.value = await getDiff(namespace.value, from.value ?? '', to.value ?? '')
  } catch (error) {
    diffFailure.value = describeFailure(error)
    diff.value = null
  } finally {
    diffLoading.value = false
  }
}

watch([tab, from, to, namespace], fetchDiff, { immediate: true })

/**
 * The tab and the compared refs are URL state, so a version diff is something a
 * colleague can be sent. `form` is the default and carries no parameter.
 */
watch([tab, from, to], () => {
  const next: Record<string, string> = {}
  if (tab.value !== 'form') next.tab = tab.value
  if (tab.value === 'versions' && from.value !== null) next.from = from.value
  if (tab.value === 'versions' && to.value !== null) next.to = to.value

  const current = route.query
  const same =
    (current.tab ?? undefined) === (next.tab ?? undefined) &&
    (current.from ?? undefined) === (next.from ?? undefined) &&
    (current.to ?? undefined) === (next.to ?? undefined)
  if (same) return
  void router.replace({ query: { ...route.query, ...next } })
})

onMounted(load)
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

      <IonToolbar v-if="!loading && failure === null">
        <IonSegment :value="tab" @update:model-value="onTabChange">
          <IonSegmentButton value="form">Form</IonSegmentButton>
          <IonSegmentButton value="raw">JSON</IonSegmentButton>
          <IonSegmentButton value="versions">Versions</IonSegmentButton>
        </IonSegment>
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page">
        <PageHeader :title="form?.title ?? namespace">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <RoleGate role="live_ops">
              <IonButton data-testid="save-draft" :disabled="!canSave" @click="save">
                <IonSpinner v-if="saving" name="dots" />
                <span v-else>Save draft</span>
              </IonButton>
              <IonButton fill="clear" color="medium" :disabled="!dirty || saving" @click="revert">
                Discard my changes
              </IonButton>
              <IonButton
                data-testid="create-version"
                :disabled="loading || failure !== null || saving"
                @click="openCreate"
              >
                Create version
              </IonButton>
            </RoleGate>
          </template>
        </PageHeader>

        <div v-if="loading" class="editor__busy">
          <IonSpinner name="dots" />
          <p>Loading {{ namespace }}...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="load"
        />

        <template v-else>
          <Toolbar class="editor__status">
            <span
              class="editor__chip"
              :class="dirty ? 'editor__chip--warn' : 'editor__chip--ok'"
              data-testid="status-unsaved"
            >
              {{ dirty ? `Unsaved changes (${changes.length})` : 'No unsaved changes' }}
            </span>
            <span class="editor__chip" data-testid="status-revision">draft rev {{ revision }}</span>
            <span v-if="baseVersion > 0" class="editor__chip" data-testid="status-base">
              based on v{{ baseVersion }}
            </span>
            <span v-if="schema" class="editor__chip">schema v{{ schema.schemaVersion }}</span>
            <CopyableId :value="namespace" label="namespace" />
            <span v-if="updatedBy !== ''" class="editor__chip"
              >saved by {{ updatedBy }} at {{ updatedAt }}</span
            >
          </Toolbar>

          <p
            v-if="versionToast !== null"
            class="editor__success"
            role="status"
            data-testid="editor-toast"
          >
            {{ versionToast }}
          </p>

          <p v-if="form?.fromEnvelope === false" class="editor__note">
            No presentation metadata for this namespace, so the form follows the schema's own
            property order and titles. Nothing is missing: a namespace without an envelope still
            edits fully.
          </p>
          <p v-else-if="form?.fromEnvelope === true" class="editor__note">
            An entry in the <code>ui.presentation</code> namespace is applied here. Whatever it does
            not name keeps the schema's own order and title.
          </p>

          <!-- The raw tab. A text that does not parse keeps the document at its
               last parseable state and says so, rather than emptying it. -->
          <template v-if="tab === 'raw'">
            <RawJsonEditor
              :model-value="rawText"
              :readonly="!canEdit"
              :aria-label="`${namespace} document as JSON`"
              @update:model-value="onRawInput"
            />
            <ErrorPanel v-if="rawProblem !== null" :message="rawProblem" class="editor__block" />
          </template>

          <!-- The history tab. Readable by viewers; the diff is fetched from
               Config, and a row plus Draft is a valid pair. -->
          <template v-else-if="tab === 'versions'">
            <VersionsTab
              :key="`${namespace}:${versionsRefresh}`"
              :namespace="namespace"
              :from="from"
              :to="to"
              @select="onSelectVersion"
            />

            <Panel v-if="comparing" title="Differences" class="editor__diff">
              <p v-if="diffLoading" class="editor__hint">Loading the diff...</p>
              <ErrorPanel
                v-else-if="diffFailure !== null"
                :message="diffFailure.summary"
                :detail="diffFailure.detail"
                :request-id="diffFailure.reference"
              />
              <DiffView
                v-else-if="diff !== null"
                :changes="diff.changes"
                :from-ref="diff.from.ref"
                :to-ref="diff.to.ref"
                :from-document="diff.from.document"
                :to-document="diff.to.document"
              />
            </Panel>
            <p v-else class="editor__hint">
              Select two rows above, or a row and Draft, to see their differences.
            </p>
          </template>

          <template v-else>
            <EmptyState
              v-if="form !== null && form.sections.length === 0"
              title="This schema has no properties"
              detail="The document is edited as JSON."
            />
            <Panel
              v-for="(section, index) in form?.sections ?? []"
              :key="index"
              :title="section.label ?? undefined"
              class="editor__section"
            >
              <SchemaField
                v-for="field in section.fields"
                :key="field.name"
                :field="field"
                :pointer="`/${field.name}`"
                :value="fieldOf(document, field.name)"
                :issues="issues"
                :disabled="saving || !canEdit"
                @update:value="(value) => setField(field.name, value)"
              />
            </Panel>

            <!-- Issues no field could show: a key the schema does not define, or
                 a complaint about the document as a whole. -->
            <Panel
              v-if="unclaimed.length > 0"
              title="Problems with the document as a whole"
              class="editor__section"
            >
              <p
                v-for="issue in unclaimed"
                :key="issue.pointer + issue.message"
                class="editor__message editor__message--danger"
              >
                <code>{{ issue.pointer === '' ? '(document)' : issue.pointer }}</code>
                {{ issue.message }}
              </p>
            </Panel>

            <p v-if="schemaUnavailable !== null" class="editor__note">
              This document was not checked locally ({{ schemaUnavailable }}), so the only
              validation is Config's, on save.
            </p>

            <ErrorPanel
              v-if="saveFailure !== null"
              :message="saveFailure.summary"
              :detail="saveFailure.detail"
              :request-id="saveFailure.reference"
              class="editor__block"
            />

            <p v-if="savedAt !== null" class="editor__success">Saved at {{ savedAt }}.</p>

            <!-- The local diff: what this editor is holding that the draft is not.
                 It is the same list the conflict dialog shows, because it is the
                 same question ("what am I about to save, or lose"). -->
            <Panel class="editor__changes">
              <button
                type="button"
                class="editor__changes-toggle"
                @click="showChanges = !showChanges"
              >
                <span v-if="dirty">Unsaved changes ({{ changes.length }})</span>
                <span v-else>No unsaved changes</span>
              </button>
              <ul v-if="showChanges && dirty" class="editor__change-list">
                <li v-for="change in changes" :key="`${change.kind}:${change.pointer}`">
                  <CopyableId :value="formatPointer(change.pointer)" label="pointer" />
                  <span class="editor__change-kind">{{ change.kind }}</span>
                </li>
              </ul>
            </Panel>

            <p v-if="rawProblem !== null" class="editor__hint">
              Saving is unavailable while the JSON does not parse: the text on screen is not a
              document yet.
            </p>
            <p v-else-if="!dirty" class="editor__hint">
              Save becomes available once something changes.
            </p>
          </template>
        </template>
      </div>
    </IonContent>

    <ConflictDialog
      :is-open="conflict !== null"
      :local-revision="revision"
      :server-revision="conflict?.revision ?? 0"
      :server-updated-by="conflict?.updatedBy ?? ''"
      :server-updated-at="conflict?.updatedAt ?? ''"
      :changes="changes"
      :pending="saving"
      @dismiss="conflict = null"
      @reload="reloadFromServer"
      @overwrite="overwrite"
    />

    <CreateVersionDialog
      v-if="canEdit"
      :is-open="createOpen"
      :namespace="namespace"
      :revision="revision"
      :local-changes="changes"
      @created="onVersionCreated"
      @dismiss="createOpen = false"
      @reload-draft="onReloadDraftFromCreate"
    />
  </IonPage>
</template>

<style scoped>
.editor__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.editor__status {
  margin-bottom: var(--ds-space-4);
}

.editor__chip {
  align-items: center;
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  display: inline-flex;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.editor__chip--ok {
  background: var(--ds-ok-soft);
  color: var(--ds-ok);
}

.editor__chip--warn {
  background: var(--ds-warn-soft);
  color: var(--ds-warn);
}

.editor__block {
  margin-top: var(--ds-space-4);
}

.editor__note,
.editor__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.editor__section,
.editor__diff {
  margin-top: var(--ds-space-5);
}

.editor__message {
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0;
}

.editor__message--danger {
  color: var(--ds-danger);
}

.editor__success {
  color: var(--ds-ok);
  font-size: var(--ds-font-size-sm);
}

.editor__changes {
  margin-top: var(--ds-space-5);
}

.editor__changes-toggle {
  background: none;
  border: 0;
  color: var(--ds-text);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: 0;
}

.editor__change-list {
  list-style: none;
  margin: var(--ds-space-2) 0 0;
  padding: 0;
}

.editor__change-list li {
  align-items: center;
  border-top: 1px solid var(--ds-border);
  display: flex;
  flex-wrap: wrap;
  gap: var(--ds-space-2);
  padding: var(--ds-space-2) 0;
}

.editor__change-kind {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
}
</style>
