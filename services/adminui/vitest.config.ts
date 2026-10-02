import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// Unit tests only. The end-to-end suite is Playwright (playwright.config.ts),
// and it runs in mock mode so it needs no backend.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    // happy-dom rather than jsdom: jsdom 30 raised its engine floor to
    // ^24.15.0, which the pinned Node 24 line does not guarantee, and these
    // tests are mostly logic (the API client, the refresh coordinator, role
    // gating) rather than deep DOM fidelity.
    environment: 'happy-dom',
    include: ['tests/unit/**/*.spec.ts'],
    // Starts the shared fixture API (src/mocks/handlers.ts) for every suite, so
    // the API client, the refresh coordinator and the stores are all tested
    // against the same answers the browser gets in mock mode.
    setupFiles: ['tests/unit/setup/msw.ts'],
    // The scaffold commit has no tests yet; an empty suite is not a failure.
    passWithNoTests: true,
    restoreMocks: true,
  },
})
