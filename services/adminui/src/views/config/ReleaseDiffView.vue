<script setup lang="ts">
import { IonContent, IonHeader, IonMenuButton, IonPage, IonSpinner, IonToolbar } from '@ionic/vue'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'

import { getPreviousRelease, getRelease, type Release } from '@/api/config'
import { ApiError } from '@/api/errors'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { Breadcrumbs, ErrorPanel, PageHeader, Panel } from '@/components/ui'
import ReleaseDiffPanel from '@/config/ReleaseDiffPanel.vue'
import { diffManifests, diffServerManifests, type ManifestDiff } from '@/config/releaseDiff'

/**
 * One release against the release it was built on, from the audit's "View
 * changes" link.
 *
 * The manifest is the whole comparison, so this is a read of two releases and a
 * pure diff: no second endpoint and no diffing on the server. The base comes from
 * the link's `?base=`, which is the base the audit row recorded; a visitor who
 * lands here without one gets the release just below it on the channel instead,
 * and a first release has no base at all, which reads as "everything is new".
 *
 * A changed namespace links to the namespace editor. The versions/diff tab that
 * would land on the actual change is a later change; until then the link
 * lands on the editor's default tab, which is still the right document.
 */

const route = useRoute()

const release = ref<Release | null>(null)
const base = ref<Release | null>(null)
const diff = ref<ManifestDiff | null>(null)
const serverDiff = ref<ManifestDiff | null>(null)
const loading = ref(true)
const failure = ref<FailureMessage | null>(null)

const title = computed(() =>
  release.value === null ? 'Release' : `Release ${release.value.releaseId}`,
)

function timestamp(value: string): string {
  const at = Date.parse(value)
  return Number.isNaN(at) ? value : new Date(at).toLocaleString()
}

/**
 * The base the page compares against. The link names it; a direct visit falls
 * back to the release just below this one. A base that is genuinely absent (the
 * first release on a channel) is null and every entry reads as added.
 */
async function resolveBase(channel: string, releaseId: number): Promise<Release | null> {
  const raw = route.query.base
  const fromQuery = typeof raw === 'string' && raw !== '' ? Number(raw) : Number.NaN
  if (Number.isInteger(fromQuery) && fromQuery > 0) {
    try {
      return await getRelease(channel, fromQuery)
    } catch (error) {
      // A stale link can name a base that is gone; the release itself is still
      // worth showing, and "no base" is the honest fallback.
      if (error instanceof ApiError && error.code === 'not_found') return null
      throw error
    }
  }
  return getPreviousRelease(channel, releaseId)
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  release.value = null
  base.value = null
  diff.value = null
  serverDiff.value = null

  const channel = String(route.params.channel ?? '')
  const releaseId = Number(route.params.releaseId)
  if (channel === '' || !Number.isInteger(releaseId) || releaseId < 1) {
    failure.value = { summary: 'That release id is not valid.', detail: '', reference: null }
    loading.value = false
    return
  }

  try {
    const next = await getRelease(channel, releaseId)
    release.value = next
    base.value = await resolveBase(channel, releaseId)
    diff.value = diffManifests(base.value?.manifest ?? null, next.manifest)
    // A null server manifest is a pre-CF-1 release, so there is nothing to
    // compare; the null message says that rather than showing an empty diff.
    serverDiff.value =
      next.serverManifest === null
        ? null
        : diffServerManifests(base.value?.serverManifest ?? null, next.serverManifest)
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

watch(
  () => [route.params.channel, route.params.releaseId, route.query.base],
  () => {
    void load()
  },
)

onMounted(load)
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <!-- Opens the shell's navigation below the `when="md"`
             breakpoint, where the menu is a drawer. -->
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="release-diff-page">
        <PageHeader :title="title">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
        </PageHeader>

        <div v-if="loading" class="release__busy">
          <IonSpinner name="dots" />
          <p>Loading the release...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="load"
        />

        <template v-else-if="release !== null && diff !== null">
          <Panel title="Release">
            <dl class="release__meta">
              <dt>Channel</dt>
              <dd>{{ release.channel }}</dd>
              <dt>Release</dt>
              <dd>{{ release.releaseId }}</dd>
              <dt>Message</dt>
              <dd>{{ release.message }}</dd>
              <dt>Created</dt>
              <dd>{{ release.createdBy }} at {{ timestamp(release.createdAt) }}</dd>
              <dt>Min client version</dt>
              <dd>
                <span v-if="diff.minClientVersion.changed">
                  {{ diff.minClientVersion.before ?? 'none' }} → {{ diff.minClientVersion.after }}
                </span>
                <span v-else>{{ diff.minClientVersion.after }}</span>
              </dd>
              <dt>Base</dt>
              <dd>
                <span v-if="base === null">none (first release on this channel)</span>
                <span v-else>release {{ base.releaseId }}</span>
              </dd>
            </dl>
          </Panel>

          <ReleaseDiffPanel :diff="diff" />

          <section class="release__server">
            <h2 class="release__server-title">Server manifest</h2>
            <ReleaseDiffPanel v-if="serverDiff !== null" :diff="serverDiff" />
            <p v-else class="release__server-empty" data-testid="release-server-empty">
              No server manifest (released before server manifests existed)
            </p>
          </section>
        </template>
      </div>
    </IonContent>
  </IonPage>
</template>

<style scoped>
.release__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.release__meta {
  display: grid;
  gap: var(--ds-space-1) var(--ds-space-4);
  grid-template-columns: max-content 1fr;
  margin: 0;
}

.release__meta dt {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.release__meta dd {
  margin: 0;
}

.release__server {
  margin-top: var(--ds-space-6);
}

.release__server-title {
  color: var(--ds-text);
  font-size: var(--ds-font-size-lg);
  line-height: var(--ds-line-height-tight);
  margin: 0;
}

.release__server-empty {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
}
</style>
