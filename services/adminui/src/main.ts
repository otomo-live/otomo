import { createPinia } from 'pinia'
import { IonicVue } from '@ionic/vue'
import { createApp } from 'vue'

/* Ionic's required CSS. Core first, then the optional utilities. */
import '@ionic/vue/css/core.css'
import '@ionic/vue/css/normalize.css'
import '@ionic/vue/css/structure.css'
import '@ionic/vue/css/typography.css'
import '@ionic/vue/css/padding.css'
import '@ionic/vue/css/float-elements.css'
import '@ionic/vue/css/text-alignment.css'
import '@ionic/vue/css/text-transformation.css'
import '@ionic/vue/css/flex-utils.css'
import '@ionic/vue/css/display.css'
/* Follows the OS setting rather than assuming light. */
import '@ionic/vue/css/palettes/dark.system.css'

import '@/theme/tokens.css'
import '@/theme/variables.css'

import App from '@/App.vue'
import { configureClient } from '@/api/client'
import { loadEnvConfig } from '@/api/env'
import { router } from '@/router'

const app = createApp(App)
/**
 * Mock mode: the fixture API instead of a gateway, turned on by `npm run
 * dev:mock` (Vite mode `mock`, so .env.mock is what sets the flag). Never on for
 * a build.
 */
async function enableMocking(): Promise<void> {
  if (import.meta.env.VITE_ENABLE_MOCK !== '1') return

  // Dynamic, so MSW and the fixtures are not in a production bundle at all: the
  // build-time check is false there and nothing imports this chunk.
  const { worker } = await import('@/mocks/browser')

  // Both halves of this are load bearing, and both are about the base. The
  // worker is served at the origin root rather than under /admin/ (the plugin in
  // vite.config.ts explains why that needs doing outside Vite's middleware
  // chain), so `url` is root-relative and BASE_URL must not be used here. The
  // scope has to be '/' for the same reason the URL does: /api/admin/* and
  // /admin-auth/* are outside /admin/, and a worker scoped to /admin/ would not
  // see a single request the mock exists to answer.
  await worker.start({
    serviceWorker: { url: '/mockServiceWorker.js', options: { scope: '/' } },
    onUnhandledRequest(request, print) {
      // Vite's own module and asset requests are not this mock's business, and
      // warning about them buries the warning that IS the point of 'warn': a
      // call to one of the API prefixes that no fixture answers. Those keep the
      // full warning; everything else is left alone.
      const { pathname } = new URL(request.url)
      if (!pathname.startsWith('/api/') && !pathname.startsWith('/admin-auth/')) return
      print.warning()
    },
  })
}

/**
 * What a visitor sees when the app cannot start at all.
 *
 * Plain DOM with inline styles, because the failure it exists for is the one
 * where the bundle or its stylesheet did not arrive intact: pulling in Ionic to
 * render a message about a partial load would be the wrong dependency. A blank
 * page is the alternative, and it tells whoever is looking at it nothing.
 */
function renderStartupFailure(error: unknown): void {
  const host = document.querySelector('#app')
  if (host === null) return

  const panel = document.createElement('div')
  panel.style.cssText =
    'max-width:44rem;margin:4rem auto;padding:0 1.5rem;font-family:system-ui,sans-serif;line-height:1.5'

  const heading = document.createElement('h1')
  heading.textContent = 'Otomo Admin could not start'
  heading.style.cssText = 'font-size:1.25rem;margin:0 0 0.75rem'

  const detail = document.createElement('p')
  detail.textContent = error instanceof Error ? error.message : String(error)

  const hint = document.createElement('p')
  hint.textContent =
    'This is a configuration failure rather than a sign-in problem: the runtime configuration at /admin/env.json could not be read.'
  hint.style.cssText = 'color:var(--ds-text-muted)'

  panel.append(heading, detail, hint)
  host.replaceChildren(panel)
  // The console keeps the original Error, stack and cause, which the panel has
  // no room for and a developer will want.
  console.error(error)
}

async function bootstrap(): Promise<void> {
  // Before enableMocking: env.json is a static file, and starting the worker
  // first would put the request in front of a service worker that has no handler
  // for it.
  const config = await loadEnvConfig()
  configureClient({ baseUrl: config.apiBaseUrl })
  document.title = config.appTitle

  await enableMocking()

  // The app is assembled HERE rather than at module scope, and the order is the
  // reason. Installing the router starts its first navigation immediately, whose
  // guard calls `/admin-auth/refresh` to restore a session; with `use(router)`
  // at module scope that request left before `enableMocking` had a chance to
  // start the worker, so it fell through to the dev proxy and died on a gateway
  // that is not running. It looked exactly like an expired session, and it made
  // a reload in mock mode lose a session that was still stored.
  app.use(IonicVue).use(createPinia()).use(router)

  // Wait for the router so a deep link renders on first paint instead of
  // flashing the redirect target.
  await router.isReady()
  app.mount('#app')
}

void bootstrap().catch(renderStartupFailure)
