<script setup lang="ts">
import { IonContent, IonHeader, IonMenuButton, IonPage, IonSpinner, IonToolbar } from '@ionic/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import {
  getChannelHead,
  getRelease,
  isNoChanges,
  isStaleRelease,
  listReleaseHistory,
  promoteRelease,
  rollbackRelease,
  type Release,
  type ReleaseHistoryPage,
  type ReleaseHistoryRow,
} from '@/api/config'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { Breadcrumbs, DataTable, EmptyState, ErrorPanel, PageHeader, Panel } from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import RoleGate from '@/components/RoleGate.vue'
import PromoteReleaseDialog from '@/config/PromoteReleaseDialog.vue'
import RollbackReleaseDialog from '@/config/RollbackReleaseDialog.vue'
import StaleReleaseDialog from '@/config/StaleReleaseDialog.vue'
import { diffManifests, type ManifestDiff } from '@/config/releaseDiff'

/**
 * The release history: one channel at a time, chosen by `?channel=`, and the two
 * admin mutations that move a head.
 *
 * The head is `head_release_id`, never the first row: a rollback makes an older
 * release current without creating a new one, so the newest id and the head are
 * different things from that moment on. Reading the head from the id is what
 * makes the page correct after a rollback rather than merely refreshed.
 *
 * Rollback and promote both name the head they were read against as
 * `base_release_id`. A channel that moved under the operator is the existing
 * `StaleReleaseDialog`; a request that would change nothing is an inline
 * "Already the head" rather than a failure. Neither button renders below admin.
 */

const CHANNELS = ['dev', 'staging', 'live'] as const
type Channel = (typeof CHANNELS)[number]
const PAGE_SIZE = 10

const route = useRoute()

const channel = computed<Channel>(() => {
  const raw = Array.isArray(route.query.channel) ? route.query.channel[0] : route.query.channel
  return raw === 'staging' || raw === 'live' ? raw : 'dev'
})
const isLive = computed(() => channel.value === 'live')
/** The channel a promotion copies from; null for dev, which has none. */
const sourceChannel = computed<'dev' | 'staging' | null>(() => {
  if (channel.value === 'staging') return 'dev'
  if (channel.value === 'live') return 'staging'
  return null
})

const columns: DataTableColumn[] = [
  { key: 'releaseId', label: 'Release' },
  { key: 'message', label: 'Message' },
  { key: 'createdBy', label: 'Author' },
  { key: 'createdAt', label: 'Created' },
  { key: 'minClientVersion', label: 'Min client version' },
  { key: 'actions', label: 'Actions' },
]

const loading = ref(true)
const loadingOlder = ref(false)
const failure = ref<FailureMessage | null>(null)
const headReleaseId = ref(0)
const head = ref<Release | null>(null)
const rows = ref<ReleaseHistoryRow[]>([])
const nextBefore = ref<number | null>(null)

const toast = ref<string | null>(null)
const actionError = ref<string | null>(null)
const noChanges = ref(false)
const stale = ref<{ from: number; to: number } | null>(null)

const rollbackTarget = ref<ReleaseHistoryRow | null>(null)
const rollingBack = ref(false)

const promoteOpen = ref(false)
const promoteLoading = ref(false)
const promotePending = ref(false)
const promoteError = ref<string | null>(null)
const promoteSource = ref<Release | null>(null)

/** Head -> target: what the channel would serve after the rollback. */
const rollbackDiff = computed<ManifestDiff | null>(() =>
  head.value === null || rollbackTarget.value === null
    ? null
    : diffManifests(head.value.manifest, rollbackTarget.value.manifest),
)

/** Target head -> source head: what the promoted release would change. */
const promoteDiff = computed<ManifestDiff | null>(() =>
  head.value === null || promoteSource.value === null
    ? null
    : diffManifests(head.value.manifest, promoteSource.value.manifest),
)

function diffPath(ch: string, releaseId: number): string {
  return `/config/releases/${encodeURIComponent(ch)}/${releaseId}`
}

function formatWhen(value: string): string {
  const at = Date.parse(value)
  return Number.isNaN(at) ? value : new Date(at).toLocaleString()
}

function isHeadRow(row: ReleaseHistoryRow): boolean {
  return row.isHead || row.releaseId === headReleaseId.value
}

async function resolveHead(ch: Channel, page: ReleaseHistoryPage): Promise<Release | null> {
  if (page.headReleaseId === 0) return null
  const inPage = page.releases.find((row) => row.releaseId === page.headReleaseId)
  return inPage ?? (await getRelease(ch, page.headReleaseId))
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const page = await listReleaseHistory(channel.value, { limit: PAGE_SIZE })
    headReleaseId.value = page.headReleaseId
    rows.value = page.releases
    nextBefore.value = page.nextBefore
    head.value = await resolveHead(channel.value, page)
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

async function loadOlder(): Promise<void> {
  const cursor = nextBefore.value
  if (cursor === null) return
  loadingOlder.value = true
  actionError.value = null
  try {
    const page = await listReleaseHistory(channel.value, { before: cursor, limit: PAGE_SIZE })
    rows.value = [...rows.value, ...page.releases]
    nextBefore.value = page.nextBefore
  } catch (error) {
    actionError.value = describeFailure(error).summary
  } finally {
    loadingOlder.value = false
  }
}

function resetActionState(): void {
  toast.value = null
  actionError.value = null
  noChanges.value = false
  stale.value = null
  rollbackTarget.value = null
  promoteOpen.value = false
  promoteSource.value = null
}

watch(channel, () => {
  resetActionState()
  void load()
})

onMounted(load)

/** Config's `A → B` stale sentence, when it is spelled the way it documents. */
function movedTo(error: unknown): number | null {
  const text = error instanceof Error ? error.message : ''
  const match = /from release \d+ to (\d+)/.exec(text)
  return match === null ? null : Number(match[1])
}

async function handleRefusal(error: unknown, baseId: number): Promise<void> {
  if (isStaleRelease(error)) {
    let to = movedTo(error)
    if (to === null) {
      try {
        to = (await getChannelHead(channel.value)).headReleaseId
      } catch {
        to = baseId
      }
    }
    stale.value = { from: baseId, to }
    return
  }
  if (isNoChanges(error)) {
    noChanges.value = true
    return
  }
  actionError.value = describeFailure(error).summary
}

function openRollback(row: ReleaseHistoryRow): void {
  actionError.value = null
  noChanges.value = false
  rollbackTarget.value = row
}

async function confirmRollback(): Promise<void> {
  const target = rollbackTarget.value
  if (target === null || head.value === null) return
  const baseId = head.value.releaseId
  rollingBack.value = true
  try {
    const release = await rollbackRelease(channel.value, {
      release_id: target.releaseId,
      base_release_id: baseId,
    })
    rollbackTarget.value = null
    toast.value = `Rolled back ${channel.value} to release ${release.releaseId}`
    await load()
  } catch (error) {
    rollbackTarget.value = null
    await handleRefusal(error, baseId)
  } finally {
    rollingBack.value = false
  }
}

async function openPromote(): Promise<void> {
  const source = sourceChannel.value
  if (source === null || head.value === null) return
  promoteOpen.value = true
  promoteLoading.value = true
  promoteError.value = null
  promoteSource.value = null
  actionError.value = null
  noChanges.value = false
  try {
    const info = await getChannelHead(source)
    if (info.release === null) {
      promoteError.value = `${source} has no releases to promote`
      return
    }
    promoteSource.value = info.release
  } catch (error) {
    promoteError.value = describeFailure(error).summary
  } finally {
    promoteLoading.value = false
  }
}

async function confirmPromote(message: string): Promise<void> {
  const source = sourceChannel.value
  const from = promoteSource.value
  if (source === null || from === null || head.value === null) return
  const baseId = head.value.releaseId
  promotePending.value = true
  try {
    const release = await promoteRelease(channel.value, source, {
      base_release_id: baseId,
      message,
    })
    promoteOpen.value = false
    toast.value = `Promoted ${source} release ${from.releaseId} to ${channel.value} as release ${release.releaseId}`
    await load()
  } catch (error) {
    promoteOpen.value = false
    await handleRefusal(error, baseId)
  } finally {
    promotePending.value = false
  }
}

async function reviewStale(): Promise<void> {
  stale.value = null
  await load()
}
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="release-history-page">
        <PageHeader title="Releases">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
        </PageHeader>

        <nav class="releases__tabs" aria-label="Channel">
          <RouterLink
            v-for="tab in CHANNELS"
            :key="tab"
            class="releases__tab"
            :class="{ 'releases__tab--active': tab === channel }"
            :to="{ path: '/config/releases', query: { channel: tab } }"
            :data-testid="`channel-tab-${tab}`"
          >
            {{ tab }}
          </RouterLink>
        </nav>

        <div v-if="loading" class="releases__busy">
          <IonSpinner name="dots" />
          <p>Loading the {{ channel }} releases...</p>
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
          <p
            v-if="toast !== null"
            class="releases__toast"
            role="status"
            data-testid="release-toast"
          >
            {{ toast }}
          </p>
          <p
            v-if="actionError !== null"
            class="releases__error"
            role="alert"
            data-testid="release-action-error"
          >
            {{ actionError }}
          </p>
          <p v-if="noChanges" class="releases__hint" data-testid="release-no-changes">
            Already the head
          </p>

          <Panel title="Channel head" data-testid="release-head">
            <template #actions>
              <RoleGate :role="isLive ? 'admin' : 'live_ops'">
                <RouterLink
                  class="releases__compose"
                  :to="`/config/releases/${channel}/compose`"
                  data-testid="compose-release"
                >
                  Compose release
                </RouterLink>
              </RoleGate>
              <RoleGate role="admin">
                <button
                  v-if="sourceChannel !== null"
                  type="button"
                  class="releases__promote"
                  data-testid="promote-release"
                  @click="openPromote"
                >
                  Promote {{ sourceChannel }} → {{ channel }}
                </button>
              </RoleGate>
            </template>

            <template v-if="head !== null">
              <dl class="releases__meta">
                <dt>Release</dt>
                <dd data-testid="head-release-id">{{ head.releaseId }}</dd>
                <dt>Message</dt>
                <dd>{{ head.message }}</dd>
                <dt>Created</dt>
                <dd>{{ head.createdBy }} at {{ formatWhen(head.createdAt) }}</dd>
                <dt>Min client version</dt>
                <dd>{{ head.minClientVersion }}</dd>
              </dl>
              <RouterLink
                class="releases__link"
                :to="diffPath(channel, head.releaseId)"
                data-testid="head-view-changes"
              >
                View changes
              </RouterLink>
            </template>
            <EmptyState
              v-else
              title="This channel has no releases yet"
              detail="Compose the first release to open its history."
            />
          </Panel>

          <Panel title="History" :padding="false" class="releases__history">
            <DataTable :columns="columns" :rows="rows" row-key="releaseId" :loading="loadingOlder">
              <template #cell-releaseId="{ row }">
                <RouterLink class="releases__link" :to="diffPath(channel, row.releaseId)">
                  {{ row.releaseId }}
                </RouterLink>
                <span
                  v-if="isHeadRow(row)"
                  class="releases__badge"
                  data-testid="release-head-badge"
                >
                  head
                </span>
              </template>
              <template #cell-createdAt="{ row }">{{ formatWhen(row.createdAt) }}</template>
              <template #cell-actions="{ row }">
                <RoleGate role="admin">
                  <button
                    v-if="!isHeadRow(row)"
                    type="button"
                    class="releases__rollback"
                    data-testid="rollback-release"
                    @click.stop="openRollback(row)"
                  >
                    Roll back
                  </button>
                </RoleGate>
              </template>
            </DataTable>

            <div v-if="nextBefore !== null" class="releases__older">
              <button
                type="button"
                class="releases__older-button"
                :disabled="loadingOlder"
                data-testid="load-older"
                @click="loadOlder"
              >
                Load older
              </button>
            </div>
          </Panel>
        </template>
      </div>
    </IonContent>

    <RollbackReleaseDialog
      v-if="rollbackTarget !== null && rollbackDiff !== null"
      :is-open="true"
      :channel="channel"
      :head-release-id="head?.releaseId ?? 0"
      :target-release-id="rollbackTarget.releaseId"
      :diff="rollbackDiff"
      :live="isLive"
      :pending="rollingBack"
      @confirm="confirmRollback"
      @cancel="rollbackTarget = null"
    />

    <PromoteReleaseDialog
      v-if="promoteOpen"
      :is-open="true"
      :target-channel="channel"
      :source-channel="sourceChannel ?? ''"
      :source-release-id="promoteSource?.releaseId ?? null"
      :diff="promoteDiff"
      :loading="promoteLoading"
      :error="promoteError"
      :live="isLive"
      :pending="promotePending"
      @confirm="confirmPromote"
      @cancel="promoteOpen = false"
    />

    <StaleReleaseDialog
      v-if="stale !== null"
      :is-open="true"
      :channel="channel"
      :from="stale.from"
      :to="stale.to"
      @review="reviewStale"
      @cancel="stale = null"
    />
  </IonPage>
</template>

<style scoped>
.releases__tabs {
  border-bottom: 1px solid var(--ds-border);
  display: flex;
  gap: var(--ds-space-1);
  margin-bottom: var(--ds-space-4);
}

.releases__tab {
  border-bottom: 2px solid transparent;
  color: var(--ds-text-muted);
  font-weight: 600;
  padding: var(--ds-space-2) var(--ds-space-3);
  text-decoration: none;
  text-transform: capitalize;
}

.releases__tab--active {
  border-bottom-color: var(--ds-accent);
  color: var(--ds-accent);
}

.releases__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.releases__toast {
  background: var(--ds-ok-soft);
  border: 1px solid var(--ds-ok);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-ok);
  font-weight: 600;
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.releases__error {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-danger);
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.releases__hint {
  color: var(--ds-text-muted);
  font-weight: 600;
  margin-bottom: var(--ds-space-4);
}

.releases__meta {
  display: grid;
  gap: var(--ds-space-1) var(--ds-space-4);
  grid-template-columns: max-content 1fr;
  margin: 0 0 var(--ds-space-3);
}

.releases__meta dt {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.releases__meta dd {
  margin: 0;
}

.releases__history {
  margin-top: var(--ds-space-4);
}

.releases__badge {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-accent);
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  margin-left: var(--ds-space-2);
  padding: var(--ds-space-1) var(--ds-space-2);
  text-transform: uppercase;
}

.releases__compose,
.releases__link {
  color: var(--ds-accent);
}

.releases__promote,
.releases__rollback,
.releases__older-button {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-3);
}

.releases__promote {
  border-color: var(--ds-accent);
  color: var(--ds-accent);
  font-weight: 600;
}

.releases__rollback {
  color: var(--ds-danger);
}

.releases__older {
  padding: var(--ds-space-3) var(--ds-space-4);
}

.releases__older-button:disabled {
  cursor: default;
  opacity: 0.6;
}
</style>
