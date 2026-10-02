<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonMenuButton,
  IonPage,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { listPacks, uploadPack, type PackSummary, type PackUploadResult } from '@/api/config'
import { describeFailure, type FailureMessage } from '@/api/messages'
import {
  Breadcrumbs,
  CopyableId,
  DataTable,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Panel,
} from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import RoleGate from '@/components/RoleGate.vue'
import {
  derivePackName,
  formatBytes,
  isPckHeader,
  isValidPackName,
  packSizeProblem,
  PACK_MAGIC,
  uploadResultText,
} from '@/config/packUpload'

/**
 * Content packs: the raw `.pck` blobs a client downloads.
 *
 * Unlike a namespace, a pack has no document to edit: it is bytes plus a name,
 * so this page is an upload and a list. Uploading is a `live_ops` write
 * (POST /packs is live_ops in gateway_dev's table); reading the list is a
 * `viewer` one, so the table is not gated.
 *
 * The checks before the request are deliberately the same rules Config applies
 * (name grammar, `GDPC` magic, 512 MiB): a viewer on a slow connection should not
 * spend five minutes uploading a file the service will reject in the first four
 * bytes. They are a convenience, never the enforcement: Config still checks.
 */

const packs = ref<PackSummary[]>([])
const loading = ref(true)
const listFailure = ref<FailureMessage | null>(null)

const file = ref<File | null>(null)
const name = ref('')
const fileInput = ref<HTMLInputElement | null>(null)
const headerChecked = ref(false)
const headerError = ref<string | null>(null)

const uploading = ref(false)
const progress = ref({ loaded: 0, total: 0 })
const elapsed = ref(0)
const uploadFailure = ref<FailureMessage | null>(null)
const result = ref<PackUploadResult | null>(null)

let controller: AbortController | null = null
let startedAt = 0

const columns: DataTableColumn[] = [
  { key: 'name', label: 'Name' },
  { key: 'sha256', label: 'SHA-256' },
  { key: 'size', label: 'Size' },
  { key: 'uploadedBy', label: 'Uploaded by' },
  { key: 'uploadedAt', label: 'Uploaded' },
]

const nameValid = computed(() => isValidPackName(name.value))
const sizeProblem = computed(() => (file.value === null ? null : packSizeProblem(file.value.size)))

const canUpload = computed(
  () =>
    file.value !== null &&
    !uploading.value &&
    nameValid.value &&
    headerChecked.value &&
    headerError.value === null &&
    sizeProblem.value === null,
)

const newestFirst = computed(() =>
  [...packs.value].sort((left, right) => right.uploadedAt.localeCompare(left.uploadedAt)),
)

const percent = computed(() => {
  const total = progress.value.total
  if (total <= 0) return 0
  return Math.min(100, Math.round((progress.value.loaded / total) * 100))
})

const rate = computed(() => {
  if (elapsed.value <= 0) return 0
  return progress.value.loaded / elapsed.value
})

const etaText = computed(() => {
  if (rate.value <= 0) return '—'
  const remaining = (progress.value.total - progress.value.loaded) / rate.value
  return `${Math.ceil(remaining)}s`
})

const resultText = computed(() =>
  result.value === null ? '' : uploadResultText(result.value.created, result.value.pack.name),
)

function shortSha(sha256: string): string {
  return sha256.slice(0, 12)
}

function formatWhen(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function openPicker(): void {
  fileInput.value?.click()
}

async function inspectHeader(chosen: File): Promise<void> {
  headerChecked.value = false
  headerError.value = null
  try {
    const head = new Uint8Array(await chosen.slice(0, 4).arrayBuffer())
    if (!isPckHeader(head)) {
      headerError.value = `That file does not start with ${PACK_MAGIC}; it is not a Godot content pack.`
    }
  } catch {
    headerError.value = 'That file could not be read.'
  } finally {
    headerChecked.value = true
  }
}

async function selectFile(chosen: File): Promise<void> {
  file.value = chosen
  name.value = derivePackName(chosen.name)
  uploadFailure.value = null
  result.value = null
  await inspectHeader(chosen)
}

function onFileChange(event: Event): void {
  const input = event.target as HTMLInputElement
  const chosen = input.files?.[0]
  if (chosen !== undefined) void selectFile(chosen)
}

function onDrop(event: DragEvent): void {
  const dropped = event.dataTransfer?.files?.[0]
  if (dropped !== undefined) void selectFile(dropped)
}

function cancelUpload(): void {
  controller?.abort()
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

async function startUpload(): Promise<void> {
  const chosen = file.value
  if (chosen === null || !canUpload.value) return

  uploadFailure.value = null
  result.value = null
  uploading.value = true
  progress.value = { loaded: 0, total: chosen.size }
  startedAt = Date.now()
  elapsed.value = 0
  controller = new AbortController()

  try {
    result.value = await uploadPack(name.value, chosen, {
      signal: controller.signal,
      onProgress: (update) => {
        progress.value = update
        elapsed.value = (Date.now() - startedAt) / 1000
      },
    })
    await load()
  } catch (error) {
    // A cancel is the user's own decision, so it leaves no error behind.
    if (!isAbortError(error)) uploadFailure.value = describeFailure(error)
  } finally {
    uploading.value = false
    controller = null
  }
}

async function load(): Promise<void> {
  loading.value = true
  listFailure.value = null
  try {
    packs.value = await listPacks()
  } catch (error) {
    listFailure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

/** Warn before losing an upload in flight; browsers show their own wording. */
function onBeforeUnload(event: BeforeUnloadEvent): void {
  if (!uploading.value) return
  event.preventDefault()
  event.returnValue = ''
}

onMounted(() => {
  window.addEventListener('beforeunload', onBeforeUnload)
  void load()
})

onBeforeUnmount(() => {
  window.removeEventListener('beforeunload', onBeforeUnload)
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
      <div class="ds-page">
        <PageHeader title="Packs">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
        </PageHeader>

        <RoleGate role="live_ops">
          <Panel title="Upload a content pack" class="packs__upload">
            <div
              class="packs__drop"
              data-testid="pack-dropzone"
              @dragover.prevent
              @drop.prevent="onDrop"
            >
              <input
                ref="fileInput"
                class="packs__file"
                type="file"
                accept=".pck"
                aria-label="Content pack file"
                data-testid="pack-file-input"
                @change="onFileChange"
              />
              <p class="packs__drop-hint">
                Drop a <code>.pck</code> file here, or
                <button type="button" class="packs__link" @click="openPicker">choose one</button>.
              </p>
              <p v-if="file !== null" class="packs__picked" data-testid="pack-file-summary">
                {{ file.name }} — {{ formatBytes(file.size) }}
              </p>
            </div>

            <label class="packs__field">
              <span>Name</span>
              <input
                v-model="name"
                class="packs__input"
                type="text"
                name="pack-name"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="!nameValid"
                data-testid="pack-name"
              />
            </label>
            <p v-if="!nameValid" class="packs__hint" data-testid="pack-name-hint">
              A pack name is a lowercase letter followed by up to 63 lowercase letters, digits or
              underscores: <code>[a-z][a-z0-9_]{0,63}</code>.
            </p>

            <p v-if="headerError !== null" class="packs__problem" data-testid="pack-file-error">
              {{ headerError }}
            </p>
            <p v-if="sizeProblem !== null" class="packs__problem" data-testid="pack-size-error">
              {{ sizeProblem }}
            </p>

            <div v-if="uploading" class="packs__progress-block">
              <div
                class="packs__progress"
                role="progressbar"
                aria-label="Upload progress"
                aria-valuemin="0"
                aria-valuemax="100"
                :aria-valuenow="percent"
                data-testid="pack-progress"
              >
                <div class="packs__progress-fill" :style="{ width: `${percent}%` }" />
              </div>
              <p class="packs__progress-text" data-testid="pack-progress-text">
                {{ percent }}% — {{ formatBytes(progress.loaded) }} /
                {{ formatBytes(progress.total) }} — {{ formatBytes(rate) }}/s — ETA {{ etaText }}
              </p>
              <IonButton fill="clear" data-testid="pack-cancel" @click="cancelUpload">
                Cancel
              </IonButton>
            </div>

            <IonButton data-testid="pack-upload-submit" :disabled="!canUpload" @click="startUpload">
              Upload
            </IonButton>

            <ErrorPanel
              v-if="uploadFailure !== null"
              :message="uploadFailure.summary"
              :detail="uploadFailure.detail"
              :request-id="uploadFailure.reference"
            />

            <div v-if="result !== null" class="packs__result" data-testid="pack-result">
              <p class="packs__result-line">{{ resultText }}</p>
              <p>
                SHA-256
                <CopyableId
                  :value="result.pack.sha256"
                  :display="shortSha(result.pack.sha256)"
                  label="pack SHA-256"
                />
              </p>
              <p>Size {{ formatBytes(result.pack.size) }}</p>
            </div>
          </Panel>
        </RoleGate>

        <Panel title="Content packs" :padding="false" class="packs__list">
          <template #actions>
            <IonButton fill="clear" :disabled="loading" @click="load">Refresh</IonButton>
          </template>

          <div v-if="loading" class="packs__busy">
            <IonSpinner name="dots" />
            <p>Loading packs...</p>
          </div>

          <ErrorPanel
            v-else-if="listFailure !== null"
            :message="listFailure.summary"
            :detail="listFailure.detail"
            :request-id="listFailure.reference"
            retry-label="Try again"
            @retry="load"
          />

          <EmptyState
            v-else-if="packs.length === 0"
            title="No content packs yet"
            detail="A live_ops operator can upload the first one."
          />

          <DataTable v-else :columns="columns" :rows="newestFirst" row-key="packId">
            <template #cell-sha256="{ row }">
              <CopyableId
                :value="row.sha256"
                :display="shortSha(row.sha256)"
                label="pack SHA-256"
              />
            </template>
            <template #cell-size="{ row }">{{ formatBytes(row.size) }}</template>
            <template #cell-uploadedAt="{ row }">{{ formatWhen(row.uploadedAt) }}</template>
          </DataTable>
        </Panel>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.packs__upload {
  margin-bottom: var(--ds-space-5);
}

.packs__drop {
  border: 1px dashed var(--ds-border);
  border-radius: var(--ds-radius-md);
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-4);
  position: relative;
}

.packs__file {
  height: 1px;
  opacity: 0;
  overflow: hidden;
  position: absolute;
  width: 1px;
}

.packs__drop-hint {
  color: var(--ds-text-muted);
  margin: 0;
}

.packs__picked {
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
}

.packs__link {
  background: none;
  border: none;
  color: var(--ds-accent);
  cursor: pointer;
  font: inherit;
  padding: 0;
  text-decoration: underline;
}

.packs__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
  margin-bottom: var(--ds-space-3);
}

.packs__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  max-width: 28rem;
  padding: var(--ds-space-2);
}

.packs__input:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.packs__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.packs__problem {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-3);
}

.packs__progress-block {
  margin-bottom: var(--ds-space-3);
}

.packs__progress {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  height: 10px;
  max-width: 32rem;
  overflow: hidden;
}

.packs__progress-fill {
  background: var(--ds-accent);
  height: 100%;
  transition: width 0.15s linear;
}

.packs__progress-text {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
}

.packs__result {
  border-top: 1px solid var(--ds-border);
  margin-top: var(--ds-space-4);
  padding-top: var(--ds-space-3);
}

.packs__result-line {
  font-weight: 600;
  margin: 0 0 var(--ds-space-2);
}

.packs__result p {
  margin: var(--ds-space-1) 0;
}

.packs__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding: var(--ds-space-6);
}
</style>
