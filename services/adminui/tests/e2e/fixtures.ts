import { test as base, expect, type Page, type TestInfo } from '@playwright/test'

/**
 * The e2e harness's one piece of environmental hardening.
 *
 * The full suite runs in WSL, where the network occasionally changes under a
 * page load. Chromium aborts every in-flight request it had on that page when
 * the interface flaps and reports `net::ERR_NETWORK_CHANGED`; because the aborted
 * requests include the entry module and every stylesheet, the app never boots
 * and the first assertion after `goto` times out. That is a property of the host,
 * not of the app, so the shell suite must not be the place it is fixed. This
 * fixture only watches for that exact failure and reloads the page once (at most
 * twice) — a retry of the *navigation*, never of the assertions, and never
 * Playwright's own `retries`, which would hide a genuine first-paint race.
 *
 * The watcher is attached per page, so a helper that opens a fresh tab is covered
 * the same way the test's own page is.
 */

const FAILURE_MARKER = 'ERR_NETWORK_CHANGED'
const MAX_RELOADS = 2

/** URLs, per page, whose request failed with `ERR_NETWORK_CHANGED`. */
const networkChanges = new WeakMap<Page, string[]>()

function track(page: Page): string[] {
  const existing = networkChanges.get(page)
  if (existing !== undefined) return existing

  const failures: string[] = []
  networkChanges.set(page, failures)
  page.on('requestfailed', (request) => {
    const errorText = request.failure()?.errorText ?? ''
    if (errorText.includes(FAILURE_MARKER)) failures.push(request.url())
  })
  return failures
}

/**
 * Navigates, then repairs the one environmental failure worth repairing.
 *
 * `page.goto` resolves on the load event, which waits for the failed scripts and
 * styles to settle, so the `requestfailed` events for a flapped load have already
 * been delivered by the time it returns. If any belong to this load, the page is
 * reloaded and the count is re-read: a reload with no new failures means the app
 * has booted, and one that fails again is tried once more before the test is
 * allowed to fail honestly.
 */
function currentTestInfo(): TestInfo | undefined {
  try {
    return base.info()
  } catch {
    // Outside a running test there is nothing to annotate.
    return undefined
  }
}

export async function gotoApp(page: Page, url: string, testInfo?: TestInfo): Promise<void> {
  const info = testInfo ?? currentTestInfo()
  const failures = track(page)
  const before = failures.length
  let navigated = true

  try {
    await page.goto(url)
  } catch (error) {
    // A document-level flap throws rather than merely logging a subresource. If
    // the marker was seen it is repaired below; anything else is the test's.
    navigated = false
    if (failures.length === before) throw error
  }

  let reloads = 0
  while (failures.length > before && reloads < MAX_RELOADS) {
    reloads += 1
    info?.annotations.push({
      type: 'network-change',
      description: `ERR_NETWORK_CHANGED during ${url}; reload ${reloads} of at most ${MAX_RELOADS}`,
    })

    const beforeReload = failures.length
    try {
      if (navigated) await page.reload()
      else await page.goto(url)
      navigated = true
    } catch (error) {
      if (failures.length === beforeReload) throw error
    }
    if (failures.length === beforeReload) break
  }
}

/**
 * `test` with the page watcher installed, and `gotoApp` as a fixture so every
 * spec navigates through the repair and logs its annotation without threading
 * `test.info()` by hand. The optional second argument is the page to drive, so a
 * flow that opens a second tab is covered too.
 */
interface AppFixtures {
  gotoApp: (url: string, page?: Page) => Promise<void>
}

export const test = base.extend<AppFixtures>({
  page: async ({ page }, use) => {
    track(page)
    await use(page)
  },
  gotoApp: async ({ page }, use, testInfo) => {
    await use(async (url, target = page) => {
      await gotoApp(target, url, testInfo)
    })
  },
})

export { expect }
export type { Page, TestInfo } from '@playwright/test'
