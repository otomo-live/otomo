<script setup lang="ts">
import { IonContent, IonHeader, IonMenuButton, IonPage, IonSpinner, IonToolbar } from '@ionic/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import {
  getChannelHead,
  isStaleRelease,
  listNamespaces,
  listPacks,
  listVersions,
  publishRelease,
  type NamespaceSummary,
  type PackSummary,
  type Release,
} from '@/api/config'
import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { hasRoleAtLeast } from '@/auth/roles'
import { Breadcrumbs, EmptyState, ErrorPanel, PageHeader, Panel } from '@/components/ui'
import ReleaseDiffPanel from '@/config/ReleaseDiffPanel.vue'
import {
  buildDraftManifests,
  defaultVersionSelection,
  isValidMinClientVersion,
  manifestsHaveChanges,
  selectedHeadPacks,
  type VersionSelection,
} from '@/config/releaseCompose'
import { diffManifests, diffServerManifests } from '@/config/releaseDiff'
import StaleReleaseDialog from '@/config/StaleReleaseDialog.vue'
import { useSessionStore } from '@/stores/session'

/**
 * The release composer: one version per namespace (client and server) plus
 * packs, frozen into a channel's client and server manifests.
 *
 * The head is read first and its id remembered as `base_release_id`; the whole
 * point of that field is that Config refuses a publish whose base is no longer
 * the channel head, so a preview the reviewer saw is the thing that ships. The
 * preview diffs two would-be manifests built from the selections against the
 * head's two documents, and the server's hashes are never guessed: a namespace
 * whose version did not move keeps the head's hash so it reads unchanged, and a
 * moved one is already a change by version.
 *
 * `live` is special twice over. The route refuses non-admins, and the page says
 * so plainly rather than rendering a form nobody may submit; and publishing to
 * players needs the word typed, so the button cannot be reached by a stray click.
 */

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const channel = computed(() => String(route.params.channel ?? ''))

const loading = ref(true)
const failure = ref<FailureMessage | null>(null)
const head = ref<Release | null>(null)
const namespaces = ref<NamespaceSummary[]>([])
const packs = ref<PackSummary[]>([])
const versionOptions = ref<Record<string, number[]>>({})

const versionSelection = ref<VersionSelection>({})
const selectedPackShas = ref<string[]>([])
const minClientVersion = ref('')
const message = ref('')
const liveConfirm = ref('')

const publishing = ref(false)
const publishError = ref<string | null>(null)
const toast = ref<string | null>(null)
const stale = ref<{ from: number; to: number } | null>(null)

const isLive = computed(() => channel.value === 'live')
const isAdmin = computed(
  () => session.status === 'authenticated' && hasRoleAtLeast(session.roles, 'admin'),
)
/** Live publishing is admin-only; a live_ops session that reaches the URL is refused here. */
const liveDenied = computed(() => isLive.value && !isAdmin.value)

const clientNamespaces = computed(() =>
  namespaces.value.filter((entry) => entry.audience === 'client'),
)
const serverNamespaces = computed(() =>
  namespaces.value.filter((entry) => entry.audience === 'server'),
)

const drafts = computed(() =>
  buildDraftManifests(
    channel.value,
    head.value,
    {
      versions: versionSelection.value,
      packShas: selectedPackShas.value,
      minClientVersion: minClientVersion.value,
    },
    packs.value,
    namespaces.value,
  ),
)

const diffs = computed(() =>
  head.value === null
    ? null
    : {
        client: diffManifests(head.value.manifest, drafts.value.client),
        // A pre-feature head has no server document; the empty base makes every
        // selected server namespace read as added, which is the honest outcome.
        server: diffServerManifests(head.value.serverManifest, drafts.value.server),
      },
)

const nothingToChange = computed(
  () => diffs.value === null || !manifestsHaveChanges(diffs.value.client, diffs.value.server),
)
const minValid = computed(() => isValidMinClientVersion(minClientVersion.value))
const messageValid = computed(() => message.value.trim() !== '')
const liveConfirmValid = computed(() => !isLive.value || liveConfirm.value === 'live')

const versionArray = computed(() =>
  Object.entries(versionSelection.value)
    .filter((entry): entry is [string, number] => entry[1] !== null)
    .map(([namespace, version]) => ({ namespace, version })),
)

const canPublish = computed(
  () =>
    !loading.value &&
    failure.value === null &&
    !liveDenied.value &&
    head.value !== null &&
    !publishing.value &&
    minValid.value &&
    messageValid.value &&
    !nothingToChange.value &&
    liveConfirmValid.value,
)

/** The head's numeric id, kept after a `to` could not be read out of the refusal. */
function currentHeadId(): number {
  return head.value?.releaseId ?? 0
}

function setVersion(namespace: string, event: Event): void {
  const raw = (event.target as HTMLSelectElement).value
  versionSelection.value = {
    ...versionSelection.value,
    [namespace]: raw === '' ? null : Number(raw),
  }
}

function togglePack(sha256: string, event: Event): void {
  const checked = (event.target as HTMLInputElement).checked
  selectedPackShas.value = checked
    ? [...selectedPackShas.value, sha256]
    : selectedPackShas.value.filter((value) => value !== sha256)
}

async function load(): Promise<void> {
  // Ionic keeps this view mounted while the route moves on. A navigation out of
  // the composer (to /config/releases, which has no :channel) leaves the
  // computed channel empty, and the watch below would otherwise fire a request
  // for channel "" before the view goes away.
  if (channel.value === '') {
    loading.value = false
    return
  }
  if (liveDenied.value) {
    loading.value = false
    return
  }
  loading.value = true
  failure.value = null
  try {
    const [headInfo, namespaceList, packList] = await Promise.all([
      getChannelHead(channel.value),
      listNamespaces(),
      listPacks(),
    ])
    head.value = headInfo.release
    namespaces.value = namespaceList
    packs.value = packList

    const entries = await Promise.all(
      namespaceList.map(async (entry): Promise<[string, number[]]> => {
        try {
          const page = await listVersions(entry.name, { limit: 50 })
          return [entry.name, page.versions.map((version) => version.version)]
        } catch {
          // A namespace whose history could not be read is offered as "no
          // version yet" rather than failing the whole page.
          return [entry.name, []]
        }
      }),
    )
    versionOptions.value = Object.fromEntries(entries)
    versionSelection.value = defaultVersionSelection(namespaceList, versionOptions.value)
    selectedPackShas.value = selectedHeadPacks(headInfo.release, packList)
    minClientVersion.value = headInfo.release?.manifest.minClientVersion ?? ''
    publishError.value = null
    toast.value = null
    stale.value = null
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

/** The `A → B` of Config's stale refusal, when it is spelled the way it documents. */
function movedTo(error: unknown): number | null {
  const text = error instanceof Error ? error.message : ''
  const match = /from release \d+ to (\d+)/.exec(text)
  return match === null ? null : Number(match[1])
}

async function publish(): Promise<void> {
  if (!canPublish.value || head.value === null) return
  const baseId = head.value.releaseId
  publishing.value = true
  publishError.value = null
  try {
    const release = await publishRelease(channel.value, {
      base_release_id: baseId,
      versions: versionArray.value,
      packs: selectedPackShas.value.map((sha256) => ({ sha256 })),
      min_client_version: minClientVersion.value.trim(),
      message: message.value.trim(),
    })
    toast.value = `Release ${release.releaseId} published to ${channel.value}`
    await router.push(`/config/releases/${encodeURIComponent(channel.value)}/${release.releaseId}`)
  } catch (error) {
    if (isStaleRelease(error)) {
      let to = movedTo(error)
      if (to === null) {
        try {
          to = (await getChannelHead(channel.value)).headReleaseId
        } catch {
          to = currentHeadId()
        }
      }
      stale.value = { from: baseId, to }
    } else if (
      error instanceof ApiError &&
      (error.code === 'validation_failed' || error.code === 'not_found')
    ) {
      publishError.value = error.message
    } else {
      publishError.value = describeFailure(error).summary
    }
  } finally {
    publishing.value = false
  }
}

/** Reload the new head, keeping the selections; the preview recomputes against it. */
async function reviewHead(): Promise<void> {
  stale.value = null
  try {
    const info = await getChannelHead(channel.value)
    head.value = info.release
    versionSelection.value = { ...versionSelection.value }
  } catch (error) {
    publishError.value = describeFailure(error).summary
  }
}

watch(channel, () => {
  if (liveDenied.value) return
  void load()
})

onMounted(load)
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="release-compose-page">
        <PageHeader :title="`Compose ${channel} release`">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
        </PageHeader>

        <ErrorPanel
          v-if="liveDenied"
          message="Publishing to live is restricted to administrators."
          detail="A live release reaches players. Ask an admin to publish it, or compose for dev or staging."
          data-testid="compose-live-denied"
        />

        <div v-else-if="loading" class="compose__busy">
          <IonSpinner name="dots" />
          <p>Loading the {{ channel }} head...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="load"
        />

        <EmptyState
          v-else-if="head === null"
          title="This channel has no releases yet"
          detail="There is no head to build on, so there is nothing to compose yet."
        />

        <template v-else>
          <p v-if="toast !== null" class="compose__toast" role="status" data-testid="compose-toast">
            {{ toast }}
          </p>

          <Panel title="Channel head">
            <dl class="compose__meta">
              <dt>Release</dt>
              <dd>{{ head.releaseId }}</dd>
              <dt>Min client version</dt>
              <dd>{{ head.manifest.minClientVersion }}</dd>
              <dt>Base release id</dt>
              <dd>{{ head.releaseId }}</dd>
            </dl>
          </Panel>

          <Panel title="Namespaces" class="compose__panel">
            <p class="compose__hint">Every namespace defaults to its latest version.</p>
            <div
              v-for="entry in clientNamespaces"
              :key="entry.name"
              class="compose__row"
              data-testid="compose-namespace"
            >
              <span class="compose__name">{{ entry.name }}</span>

              <span
                v-if="(versionOptions[entry.name]?.length ?? 0) === 0"
                class="compose__grey"
                :data-testid="`compose-noversion-${entry.name}`"
              >
                no version yet
              </span>

              <select
                v-else
                class="compose__select"
                :aria-label="`Version for ${entry.name}`"
                :data-testid="`compose-version-${entry.name}`"
                :value="versionSelection[entry.name] ?? ''"
                @change="setVersion(entry.name, $event)"
              >
                <option value="">not included</option>
                <option
                  v-for="version in versionOptions[entry.name]"
                  :key="version"
                  :value="version"
                >
                  v{{ version }}
                </option>
              </select>
            </div>

            <div
              v-if="serverNamespaces.length > 0"
              class="compose__server-group"
              data-testid="compose-server-group"
            >
              <h3 class="compose__subheading">Server namespaces</h3>
              <p class="compose__hint">sent to game servers and Session only, never to clients</p>
              <div
                v-for="entry in serverNamespaces"
                :key="entry.name"
                class="compose__row"
                data-testid="compose-namespace"
              >
                <span class="compose__name">{{ entry.name }}</span>

                <span
                  v-if="(versionOptions[entry.name]?.length ?? 0) === 0"
                  class="compose__grey"
                  :data-testid="`compose-noversion-${entry.name}`"
                >
                  no version yet
                </span>

                <select
                  v-else
                  class="compose__select"
                  :aria-label="`Version for ${entry.name}`"
                  :data-testid="`compose-version-${entry.name}`"
                  :value="versionSelection[entry.name] ?? ''"
                  @change="setVersion(entry.name, $event)"
                >
                  <option value="">not included</option>
                  <option
                    v-for="version in versionOptions[entry.name]"
                    :key="version"
                    :value="version"
                  >
                    v{{ version }}
                  </option>
                </select>
              </div>
            </div>
          </Panel>

          <Panel title="Packs" class="compose__panel">
            <p v-if="packs.length === 0" class="compose__hint">No content packs are held yet.</p>
            <label
              v-for="pack in packs"
              :key="pack.sha256"
              class="compose__check"
              data-testid="compose-pack"
            >
              <input
                type="checkbox"
                :value="pack.sha256"
                :checked="selectedPackShas.includes(pack.sha256)"
                @change="togglePack(pack.sha256, $event)"
              />
              <span>{{ pack.name }}</span>
              <code class="compose__sha">{{ pack.sha256.slice(0, 12) }}…</code>
            </label>
          </Panel>

          <Panel title="Minimum client version" class="compose__panel">
            <input
              v-model="minClientVersion"
              class="compose__input"
              type="text"
              aria-label="Minimum client version"
              data-testid="compose-min-client"
              placeholder="1.0.0"
            />
            <p
              v-if="!minValid"
              class="compose__hint compose__hint--danger"
              data-testid="compose-min-hint"
            >
              Must be three dot-separated numbers, like 1.0.0.
            </p>
          </Panel>

          <Panel title="Message" class="compose__panel">
            <textarea
              v-model="message"
              class="compose__input compose__message"
              rows="3"
              aria-label="Release message"
              data-testid="compose-message"
              placeholder="What is in this release?"
            />
            <p
              v-if="!messageValid"
              class="compose__hint compose__hint--danger"
              data-testid="compose-message-hint"
            >
              A message is required.
            </p>
          </Panel>

          <Panel title="Preview" class="compose__panel">
            <p v-if="nothingToChange" class="compose__hint" data-testid="compose-nothing">
              Nothing would change.
            </p>
            <template v-if="diffs !== null">
              <h3 class="compose__subheading">Client manifest</h3>
              <ReleaseDiffPanel :diff="diffs.client" />
              <div class="compose__preview-server" data-testid="compose-preview-server">
                <h3 class="compose__subheading">Server manifest</h3>
                <ReleaseDiffPanel :diff="diffs.server" />
              </div>
            </template>
          </Panel>

          <p
            v-if="publishError !== null"
            class="compose__error"
            role="alert"
            data-testid="compose-error"
          >
            {{ publishError }}
          </p>

          <div v-if="isLive" class="compose__live" data-testid="compose-live-gate">
            <p class="compose__live-title">This publishes to players</p>
            <label class="compose__check">
              <span>Type <code>live</code> to publish to players</span>
              <input
                v-model="liveConfirm"
                class="compose__input"
                type="text"
                aria-label="Type live to confirm"
                data-testid="compose-live-confirm"
                placeholder="Type live to publish to players"
              />
            </label>
          </div>

          <div class="compose__actions">
            <button
              type="button"
              class="compose__publish"
              :class="{ 'compose__publish--danger': isLive }"
              :disabled="!canPublish"
              data-testid="compose-publish"
              @click="publish"
            >
              <IonSpinner v-if="publishing" name="dots" />
              <span v-else>Publish release</span>
            </button>
          </div>
        </template>
      </div>
    </IonContent>

    <StaleReleaseDialog
      v-if="stale !== null"
      :is-open="true"
      :channel="channel"
      :from="stale.from"
      :to="stale.to"
      @review="reviewHead"
      @cancel="stale = null"
    />
  </IonPage>
</template>

<style scoped>
.compose__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.compose__toast {
  background: var(--ds-ok-soft);
  border: 1px solid var(--ds-ok);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-ok);
  font-weight: 600;
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.compose__panel {
  margin-top: var(--ds-space-4);
}

.compose__meta {
  display: grid;
  gap: var(--ds-space-1) var(--ds-space-4);
  grid-template-columns: max-content 1fr;
  margin: 0;
}

.compose__meta dt {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.compose__meta dd {
  margin: 0;
}

.compose__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0 0 var(--ds-space-2);
}

.compose__hint--danger {
  color: var(--ds-danger);
}

.compose__subheading {
  color: var(--ds-text);
  font-size: var(--ds-font-size-md);
  margin: var(--ds-space-3) 0 var(--ds-space-1);
}

.compose__server-group {
  border-top: 1px solid var(--ds-border);
  margin-top: var(--ds-space-2);
  padding-top: var(--ds-space-1);
}

.compose__preview-server {
  margin-top: var(--ds-space-4);
}

.compose__row {
  align-items: center;
  border-top: 1px solid var(--ds-border);
  display: flex;
  gap: var(--ds-space-3);
  justify-content: space-between;
  padding: var(--ds-space-2) 0;
}

.compose__name {
  font-weight: 600;
}

.compose__grey {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  font-style: italic;
}

.compose__select,
.compose__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.compose__select:focus-visible,
.compose__input:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.compose__check {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
  padding: var(--ds-space-1) 0;
}

.compose__sha {
  color: var(--ds-text-muted);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
}

.compose__error {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-danger);
  margin-top: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.compose__live {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-md);
  color: var(--ds-danger);
  margin-top: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.compose__live-title {
  font-weight: 600;
  margin: 0 0 var(--ds-space-2);
}

.compose__actions {
  margin-top: var(--ds-space-4);
}

.compose__publish {
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
  padding: var(--ds-space-2) var(--ds-space-5);
}

.compose__publish--danger {
  background: var(--ds-danger);
  border-color: var(--ds-danger);
  color: var(--ds-danger-contrast);
}

.compose__publish:disabled {
  cursor: default;
  opacity: 0.6;
}

/* The message is prose people write, so it takes the panel's width rather than
 * the narrow width of the version inputs it shares a class with. */
.compose__message {
  box-sizing: border-box;
  min-height: 5rem;
  resize: vertical;
  width: 100%;
}
</style>
