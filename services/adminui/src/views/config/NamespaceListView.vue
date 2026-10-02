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
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { listNamespaces, type NamespaceSummary } from '@/api/config'
import { describeFailure, type FailureMessage } from '@/api/messages'
import { hasRoleAtLeast } from '@/auth/roles'
import {
  Breadcrumbs,
  DataTable,
  EmptyState,
  ErrorPanel,
  PageHeader,
  StatusDot,
} from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import RoleGate from '@/components/RoleGate.vue'
import CreateNamespaceDialog from '@/config/CreateNamespaceDialog.vue'
import { useSessionStore } from '@/stores/session'

/**
 * Every namespace this deployment has.
 *
 * A namespace is arbitrary JSON plus its own JSON Schema, so this list is the
 * only place the SPA names any of them, and it names none of them by hand: the
 * names come from Config. There is nothing here a project has to register.
 *
 * `audience` is shown because it is the one column that changes what an author
 * should expect: a `client` namespace is what the game reads, a `server` one is
 * what the backend reads (design/02-config.md rule 3), and a document edited with
 * the wrong one in mind is a mistake that costs a release.
 *
 * The name is a link for everyone: the editor route is `viewer` and renders
 * itself read-only below `live_ops`, so a viewer can follow it to read the form,
 * the JSON and the version history. Only the mutating controls inside are role
 * gated. The search box writes `?q=` so a filtered list is a URL someone can send
 * to a colleague.
 */

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const namespaces = ref<NamespaceSummary[]>([])
const loading = ref(true)
const failure = ref<FailureMessage | null>(null)
const createOpen = ref(false)

/** The channel the "Compose release" button opens. Live is admin-only. */
const composeChannel = ref('dev')
const canComposeLive = computed(
  () => session.status === 'authenticated' && hasRoleAtLeast(session.roles, 'admin'),
)

function openComposer(): void {
  void router.push(`/config/releases/${composeChannel.value}/compose`)
}

const query = ref(typeof route.query.q === 'string' ? route.query.q : '')

const filtered = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (needle === '') return namespaces.value
  return namespaces.value.filter((namespace) => namespace.name.toLowerCase().includes(needle))
})

const columns: DataTableColumn[] = [
  { key: 'name', label: 'Name' },
  { key: 'audience', label: 'Audience' },
  { key: 'version', label: 'Latest version' },
  { key: 'draft', label: 'Draft revision' },
  { key: 'status', label: 'Status' },
  { key: 'schema', label: 'Schema' },
]

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    namespaces.value = await listNamespaces()
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

function namespaceKey(namespace: NamespaceSummary): string {
  return namespace.name
}

function editorPath(namespace: NamespaceSummary): string {
  return `/config/namespaces/${encodeURIComponent(namespace.name)}`
}

function schemaPath(namespace: NamespaceSummary): string {
  return `/config/namespaces/${encodeURIComponent(namespace.name)}/schema`
}

// The name is a real link (keyboard and screen readers reach it); a click
// anywhere else on the row is a mouse shortcut to the same place. The editor
// route is `viewer`, so this is offered to every session.
function openNamespace(namespace: NamespaceSummary): void {
  void router.push(editorPath(namespace))
}

// The URL is the filter's state, replaced rather than pushed so typing a search
// does not fill the back stack with one entry per keystroke.
watch(query, (value) => {
  const current = typeof route.query.q === 'string' ? route.query.q : ''
  if (current === value) return
  const nextQuery = { ...route.query }
  if (value === '') delete nextQuery.q
  else nextQuery.q = value
  void router.replace({ query: nextQuery })
})

async function onCreated(namespace: NamespaceSummary): Promise<void> {
  createOpen.value = false
  await load()
  void router.push({
    name: 'config-namespace-schema',
    params: { name: namespace.name },
  })
}

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
      <div class="ds-page">
        <PageHeader title="Namespaces">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <input
              v-model="query"
              class="namespaces__search"
              type="search"
              aria-label="Search namespaces"
              placeholder="Search by name"
              data-testid="namespace-search"
            />
            <IonButton fill="clear" :disabled="loading" @click="load">Refresh</IonButton>
            <RoleGate role="live_ops">
              <label class="namespaces__compose">
                <span class="namespaces__compose-label">Channel</span>
                <select
                  v-model="composeChannel"
                  class="namespaces__channel"
                  aria-label="Release channel"
                  data-testid="compose-channel"
                >
                  <option value="dev">dev</option>
                  <option value="staging">staging</option>
                  <option v-if="canComposeLive" value="live">live</option>
                </select>
              </label>
              <IonButton data-testid="compose-release" @click="openComposer">
                Compose release
              </IonButton>
            </RoleGate>
            <RoleGate role="admin">
              <IonButton data-testid="create-namespace" @click="createOpen = true">
                Create namespace
              </IonButton>
            </RoleGate>
          </template>
        </PageHeader>

        <div v-if="loading" class="namespaces__busy">
          <IonSpinner name="dots" />
          <p>Loading namespaces...</p>
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
          v-else-if="namespaces.length === 0"
          title="No namespaces yet"
          detail="An admin can create the first one."
        >
          <template #actions>
            <RoleGate role="admin">
              <IonButton @click="createOpen = true">Create namespace</IonButton>
            </RoleGate>
          </template>
        </EmptyState>

        <DataTable
          v-else
          :columns="columns"
          :rows="filtered"
          :row-key="namespaceKey"
          @row-click="openNamespace"
        >
          <template #cell-name="{ row }">
            <RouterLink class="namespaces__name" :to="editorPath(row)">{{ row.name }}</RouterLink>
            <p class="namespaces__description">{{ row.description }}</p>
          </template>
          <template #cell-audience="{ row }">
            <span class="namespaces__badge">{{ row.audience }}</span>
          </template>
          <template #cell-version="{ row }">
            <span v-if="row.latestVersion > 0">v{{ row.latestVersion }}</span>
            <span v-else data-testid="namespace-version-none">&mdash;</span>
          </template>
          <template #cell-draft="{ row }">
            <span>{{ row.draft.revision }}</span>
          </template>
          <template #cell-status="{ row }">
            <StatusDot
              v-if="row.draft.hasUnpublishedChanges"
              status="warn"
              label="Unpublished changes"
            />
            <StatusDot v-else status="ok" label="No unpublished changes" />
          </template>
          <template #cell-schema="{ row }">
            <RouterLink class="namespaces__schema" :to="schemaPath(row)">Schema</RouterLink>
          </template>
          <template #empty>
            <p class="namespaces__empty">No namespaces match &ldquo;{{ query }}&rdquo;.</p>
          </template>
        </DataTable>
      </div>
    </IonContent>

    <RoleGate role="admin">
      <CreateNamespaceDialog
        :is-open="createOpen"
        @created="onCreated"
        @dismiss="createOpen = false"
      />
    </RoleGate>
  </IonPage>
</template>

<style scoped>
.namespaces__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.namespaces__search {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
  width: 14rem;
}

.namespaces__search:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.namespaces__compose {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
}

.namespaces__compose-label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.namespaces__channel {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
}

.namespaces__channel:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.namespaces__name {
  color: var(--ds-accent);
  font-weight: 600;
  text-decoration: none;
}

a.namespaces__name:hover,
a.namespaces__name:focus-visible {
  text-decoration: underline;
}

.namespaces__description {
  color: var(--ds-text-muted);
  margin: var(--ds-space-1) 0 0;
}

.namespaces__badge {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  display: inline-block;
  font-size: var(--ds-font-size-xs);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.namespaces__schema {
  color: var(--ds-accent);
}

.namespaces__empty {
  color: var(--ds-text-muted);
  margin: 0;
  text-align: center;
}
</style>
