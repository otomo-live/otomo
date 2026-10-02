import { defineConfig, devices } from '@playwright/test'

/**
 * The end-to-end smoke, against the fixture API in a real browser.
 *
 * Mock mode rather than a live gateway, because that is the only thing runnable
 * without a deployment: it still exercises what a unit test cannot, which is
 * that the app boots, the service worker answers, the router's base path is
 * right and Ionic renders. What it does not prove is anything about the real
 * gateway, and a deployment's own smoke test is where that belongs.
 */

const PORT = 5199

export default defineConfig({
  testDir: 'tests/e2e',
  // One dev server, and the fixture API's session lives in the page, so
  // parallelism would only buy flakiness here.
  fullyParallel: false,
  workers: 1,
  forbidOnly: process.env.CI !== undefined,
  // Zero as well as `reporter`: a retry would hide exactly the kind of
  // first-paint race this smoke exists to catch.
  retries: 0,
  reporter: process.env.CI === undefined ? 'list' : 'github',
  use: {
    // Trailing slash matters: a relative URL in a test resolves against it, and
    // the app lives under /admin/ rather than at the root.
    baseURL: `http://127.0.0.1:${PORT}/admin/`,
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    // --strictPort so a stale server on another port cannot make this look like
    // a pass against the wrong build, and --host 127.0.0.1 because Vite
    // otherwise binds localhost, which on a machine with IPv6 resolves to ::1
    // alone: the server starts, prints its URL, and every IPv4 probe of it
    // fails until webServer times out.
    command: `npm run dev:mock -- --port ${PORT} --strictPort --host 127.0.0.1`,
    url: `http://127.0.0.1:${PORT}/admin/`,
    reuseExistingServer: process.env.CI === undefined,
    timeout: 120_000,
  },
})
