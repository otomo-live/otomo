<script setup lang="ts">
import {
  IonBadge,
  IonButtons,
  IonContent,
  IonHeader,
  IonItem,
  IonLabel,
  IonList,
  IonMenu,
  IonMenuToggle,
  IonNote,
  IonRouterOutlet,
  IonSplitPane,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { envConfig } from '@/api/env'
import { routes } from '@/router'
import { navGroups, type NavEntry } from '@/router/nav'
import { environmentBanner } from '@/shell/environment'
import { useSessionStore } from '@/stores/session'
import RoutePalette from '@/components/RoutePalette.vue'

/**
 * The signed-in shell: an environment banner, a grouped side navigation, the
 * route palette and the router outlet.
 *
 * The navigation is derived from the route table (`@/router/nav`) rather than
 * written a second time here, so the menu and the guard cannot disagree about
 * who may see what. A route appears when it says `meta.nav`, in the group it
 * names, and only for a session whose roles reach its `meta.minRole`.
 */

const store = useSessionStore()
const route = useRoute()
const router = useRouter()

const appTitle = envConfig().appTitle
const signedIn = computed(() => store.status === 'authenticated')
const banner = computed(() => environmentBanner(envConfig().environment))
const sections = computed(() => navGroups(routes, store.roles))

const paletteOpen = ref(false)

// `navigator.platform` is the signal the task names, and the one that survives
// a user agent string rewritten by a privacy extension.
const isMac =
  typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform ?? '')
const shortcutLabel = isMac ? '⌘K' : 'Ctrl K'

/**
 * A sidebar item is current on its own route, and on a child route that names it
 * as its breadcrumb parent (the namespace editor, under the namespace list).
 */
function isActive(item: NavEntry): boolean {
  if (route.path === item.path) return true
  return item.name !== undefined && route.meta?.crumbParent === item.name
}

async function signOut(): Promise<void> {
  await store.signOut()
  await router.replace('/login')
}
</script>

<template>
  <div class="shell">
    <!--
      Full width, above the menu and the outlet, so the deployment cannot be
      mistaken whichever page is open. `role="status"` announces it to a screen
      reader once without stealing focus.
    -->
    <div
      class="shell__banner"
      :class="`shell__banner--${banner.kind}`"
      role="status"
      data-environment-banner
    >
      {{ banner.text }}
    </div>

    <!--
      `disabled` as well as the v-if below: with no menu in the tree the split
      pane has nothing to open, and this keeps it that way for the edge-swipe
      gesture, which is driven by the split pane rather than by the menu's
      presence.
    -->
    <IonSplitPane content-id="otomo-main" when="md" :disabled="!signedIn">
      <IonMenu v-if="signedIn" content-id="otomo-main" class="shell__menu">
        <IonHeader>
          <IonToolbar>
            <IonTitle class="shell__brand">{{ appTitle }}</IonTitle>
            <IonButtons slot="end">
              <button
                type="button"
                class="shell__search"
                :aria-label="`Search pages (${shortcutLabel})`"
                @click="paletteOpen = true"
              >
                Search <kbd class="shell__shortcut">{{ shortcutLabel }}</kbd>
              </button>
            </IonButtons>
          </IonToolbar>
        </IonHeader>
        <IonContent class="shell__nav">
          <nav aria-label="Sections">
            <div v-for="section in sections" :key="section.group" class="shell__group">
              <p class="shell__group-label">{{ section.label }}</p>
              <IonList>
                <!--
                  IonMenuToggle so the overlay closes after a tap on a narrow
                  screen, where the menu is a drawer rather than a column.

                  `auto-hide="false"` is not optional. The component's default is
                  to hide ITSELF while the menu is visible, which is the right
                  behaviour for the hamburger that opens the drawer and the wrong
                  one for these items: on a wide screen the split pane keeps the
                  menu visible, so every entry rendered as `display: none` and
                  the navigation looked empty. `auto-hide` controls the item's
                  own visibility, never whether a tap closes the menu, so turning
                  it off costs nothing.
                -->
                <IonMenuToggle v-for="item in section.items" :key="item.path" :auto-hide="false">
                  <IonItem
                    :router-link="item.path"
                    router-direction="root"
                    :detail="false"
                    :class="{ 'shell__item--active': isActive(item) }"
                    :aria-current="isActive(item) ? 'page' : undefined"
                  >
                    <IonLabel>{{ item.label }}</IonLabel>
                  </IonItem>
                </IonMenuToggle>
              </IonList>
            </div>
          </nav>

          <IonList>
            <IonItem lines="none">
              <IonLabel>
                <div>{{ store.displayName }}</div>
                <IonNote>{{ store.email ?? '' }}</IonNote>
              </IonLabel>
            </IonItem>
            <IonItem lines="none">
              <IonBadge v-for="role in store.roles" :key="role" slot="start" color="medium">
                {{ role }}
              </IonBadge>
              <IonNote v-if="store.roles.length === 0">no roles</IonNote>
            </IonItem>
            <IonItem
              router-link="/account"
              router-direction="root"
              :detail="false"
              data-testid="account-link"
            >
              <IonLabel>Account</IonLabel>
            </IonItem>
            <IonItem button :detail="false" @click="signOut">
              <IonLabel color="danger">Sign out</IonLabel>
            </IonItem>
          </IonList>
        </IonContent>
      </IonMenu>

      <IonRouterOutlet id="otomo-main" />
    </IonSplitPane>

    <RoutePalette v-if="signedIn" v-model:open="paletteOpen" />
  </div>
</template>

<style scoped>
/* A column so the banner keeps its height and the split pane takes the rest,
 * rather than the banner overlaying the page. */
.shell {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  min-height: 0;
}

.shell :deep(ion-split-pane) {
  flex: 1 1 auto;
  min-height: 0;
  /* Ionic positions the split pane absolutely over the whole viewport, which
   * would cover the banner; in flow it takes the space below it instead. */
  position: relative;
}

.shell__banner {
  border-bottom: 1px solid var(--ds-border);
  flex: none;
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
  letter-spacing: 0.02em;
  padding: var(--ds-space-2) var(--ds-space-4);
  text-align: center;
}

.shell__banner--danger {
  background: var(--ds-danger-soft);
  border-bottom-color: var(--ds-danger);
  color: var(--ds-danger);
}

.shell__banner--warn {
  background: var(--ds-warn-soft);
  border-bottom-color: var(--ds-warn);
  color: var(--ds-warn);
}

.shell__banner--info {
  background: var(--ds-info-soft);
  border-bottom-color: var(--ds-info);
  color: var(--ds-info);
}

/* The menu is a surface in the design system, not Ionic's idea of one. The
 * custom properties inherit into the shadow DOM, so no rule has to reach in. */
.shell__menu {
  --ion-background-color: var(--ds-surface);
  border-right: 1px solid var(--ds-border);
}

.shell__brand {
  font-weight: 600;
}

.shell__search {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  cursor: pointer;
  font-family: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-2);
}

.shell__search:hover,
.shell__search:focus-visible {
  border-color: var(--ds-accent);
  color: var(--ds-text);
}

.shell__shortcut {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text-muted);
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-xs);
  padding: 0 var(--ds-space-1);
}

.shell__nav {
  --padding-top: var(--ds-space-2);
}

.shell__group-label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  font-weight: 700;
  letter-spacing: 0.08em;
  margin: var(--ds-space-3) var(--ds-space-4) var(--ds-space-1);
  text-transform: uppercase;
}

.shell__item--active {
  --background: var(--ds-neutral-soft);
  --color: var(--ds-text);
  font-weight: 600;
}
</style>
