import { readFile } from 'node:fs/promises'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv, type Plugin } from 'vite'

// The SPA is served by gateway_dev under /admin/, and that route has no
// StripPrefix, so the browser, the proxy and nginx all see the prefix
// (services/gateway_dev/internal/router/dev.go:20 has StripPrefix unset, and
// internal/proxy/proxy.go:65 only rewrites the path when it is non-empty).
// Everything hangs off this one value: asset URLs, the router's history base,
// and the location of env.json at /admin/env.json that WEB-2 requires.
//
// The flip side is that WEB-6's literal `try_files $uri /index.html` is written
// for an app served at the origin root and is wrong here; nginx.conf uses
// /admin/index.html. See the README.
const BASE = '/admin/'

/**
 * Serves the MSW worker from the origin root, ahead of Vite's middleware chain.
 *
 * The URL is load bearing. The mocked routes are `/api/admin/*` and
 * `/admin-auth/*`, a service worker's scope defaults to the directory of its
 * script, and `base` here is `/admin/`: a worker served from
 * `/admin/mockServiceWorker.js` is scoped to `/admin/` and cannot see one mocked
 * request. Every call would fall through to the dev proxy and the mock would
 * silently do nothing. So the script is served at `/mockServiceWorker.js`, where
 * the root scope is allowed by default, and src/main.ts registers it with
 * `scope: '/'` to match.
 *
 * It cannot be a post hook returned from `configureServer`, which was the first
 * shape tried. Two of Vite 8's own middlewares decide the request before a post
 * hook runs, and both would break it:
 *
 *   - baseMiddleware answers any path outside `/admin/` with a 404, so
 *     `/mockServiceWorker.js` would never reach the hook at all;
 *   - htmlFallbackMiddleware then rewrites an unmatched path whose request
 *     accepts HTML to `/index.html`, so the file would be served as text/html
 *     and the browser would refuse to register it (unsupported MIME type).
 *
 * An appended `server.middlewares.use` inside the hook body is early enough,
 * which is not obvious: Vite runs every `configureServer` body before it installs
 * its own middlewares, so this lands in front of both. Measured rather than
 * assumed, by requesting the path from each position; the post-hook form is the
 * one that is too late. connect has no prepend API and none is needed.
 *
 * A file in `public/` would be served under the base and copied into every
 * production image, which is why the worker is generated into `mock/` instead:
 * nothing copies that directory into `dist/`.
 *
 * Read per request rather than inlined so the generated worker stays the only
 * copy.
 */
const MOCK_WORKER_PATH = fileURLToPath(new URL('./mock/mockServiceWorker.js', import.meta.url))

function mockServiceWorker(): Plugin {
  return {
    name: 'adminui-mock-service-worker',
    configureServer(server) {
      // Appended, and still early enough to precede Vite's own middlewares: see
      // the note above. This was measured from both positions rather than
      // assumed, because the intuitive answer is the wrong one.
      server.middlewares.use(
        // Annotated because connect types the layer's handle as a union, which
        // gives the parameters no contextual type.
        (request: IncomingMessage, response: ServerResponse, next: (error?: unknown) => void) => {
          if ((request.url ?? '').split('?')[0] !== '/mockServiceWorker.js') {
            next()
            return
          }
          if (request.method !== 'GET' && request.method !== 'HEAD') {
            response.statusCode = 405
            response.end()
            return
          }

          void readFile(MOCK_WORKER_PATH)
            .then((body) => {
              response.setHeader('Content-Type', 'text/javascript')
              // Served at the root, where the root scope is allowed anyway; the
              // header is here so that moving the file under the base one day
              // fails loudly in the browser instead of silently losing the scope
              // the mocked routes depend on.
              response.setHeader('Service-Worker-Allowed', '/')
              // A cached worker outlives the fixtures it serves and is very hard
              // to notice; in development it is never worth caching.
              response.setHeader('Cache-Control', 'no-store')
              response.end(body)
            })
            .catch(next)
        },
      )
    },
  }
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const proxyTarget = env.VITE_DEV_PROXY_TARGET || 'http://127.0.0.1:8090'

  return {
    base: BASE,
    plugins: [vue(), mockServiceWorker()],
    // These are imported only from lazily loaded routes (the JSON editor, the
    // service charts, the log list), so the dev server's first scan misses them,
    // discovers them on first use, re-bundles and reloads the page mid-navigation
    // ("Failed to fetch dynamically imported module"). Naming them here bundles them
    // up front. A dependency added to a lazy route belongs here too.
    optimizeDeps: {
      include: [
        // Imported only from the lazy config-editor route, so it is discovered
        // on first use and would otherwise reload the page mid-navigation.
        '@cfworker/json-schema',
        '@codemirror/language',
        '@codemirror/state',
        '@lezer/highlight',
        '@tanstack/vue-virtual',
        // Imported only from the lazy enrollment route, so it is
        // discovered on first use and would otherwise reload the page mid-flow.
        'qrcode',
        'uplot',
      ],
    },
    resolve: {
      alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
    },
    server: {
      port: 5173,
      proxy: {
        // Proxied rather than called cross-origin so the browser always sees a
        // same-origin request in development too. It has to be this way:
        // gateway_dev parses GATEWAY_DEV_CORS_ALLOWED_ORIGINS and nothing reads
        // it, so a cross-origin dev setup could not work even if we wanted it.
        '/api': { target: proxyTarget, changeOrigin: true },
        '/admin-auth': { target: proxyTarget, changeOrigin: true },
      },
    },
    build: {
      // The UI is served on a private network behind a staff login, so readable
      // stack traces are worth more than a slightly smaller bundle.
      sourcemap: true,
      // The JSON editor pulls CodeMirror and the schema validator into the
      // editor route, which is legitimately over Vite's 500 kB default. The
      // limit is raised rather than the warning left to train readers to ignore
      // every build warning; the route is lazy, so it is not on first paint.
      chunkSizeWarningLimit: 1200,
    },
  }
})
