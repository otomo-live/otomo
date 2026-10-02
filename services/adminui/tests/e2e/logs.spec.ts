import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The log explorer end to end: the list virtualises at ten thousand lines, the
 * live tail streams at a forced rate without unbounded growth, and Stop really
 * stops.
 *
 * The fixture's scale switches are localStorage flags, set before the app boots
 * with `addInitScript` (see the log handlers in `src/mocks/handlers.ts`).
 */

async function signIn(page: Page): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill('admin@example.com')
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

async function lineCount(page: Page): Promise<number> {
  const raw = await page.getByTestId('logs-page').getAttribute('data-line-count')
  return Number(raw ?? '0')
}

async function startTail(page: Page): Promise<void> {
  await page.getByTestId('tail-toggle').click()
  await expect(page.getByTestId('tail-toggle')).toHaveText(/Stop/)
}

test('ten thousand lines scroll with fewer than two hundred row elements', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('otomo.mock.logs', 'many')
  })
  await signIn(page)
  await gotoApp(page, '/admin/dashboard/logs')

  await expect(page.getByTestId('log-row').first()).toBeVisible()
  await expect(page.getByTestId('filter-range')).toHaveValue('1h')

  // Walk the pages until the ten thousand are in memory. The button disappears
  // when there is nothing older left.
  for (let index = 0; index < 30; index += 1) {
    const loadMore = page.getByTestId('load-more')
    if ((await loadMore.count()) === 0) break
    const before = await lineCount(page)
    await loadMore.click()
    await expect.poll(() => lineCount(page), { timeout: 15_000 }).toBeGreaterThan(before)
  }

  expect(await lineCount(page)).toBe(10_000)
  expect(await page.getByTestId('log-row').count()).toBeLessThan(200)

  const scroll = page.getByTestId('log-scroll')
  await scroll.evaluate((element) => {
    element.scrollTop = element.scrollHeight
  })

  // Scrolling completed: the last index is rendered, and the box is near the end.
  await expect
    .poll(
      () =>
        page
          .locator('[data-testid="log-row"]')
          .evaluateAll((rows) =>
            Math.max(...rows.map((row) => Number(row.getAttribute('data-index')))),
          ),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(9_900)

  const geometry = await scroll.evaluate((element) => ({
    top: element.scrollTop,
    height: element.scrollHeight,
    client: element.clientHeight,
  }))
  expect(geometry.top + geometry.client).toBeGreaterThan(geometry.height - 200)

  // Still bounded after the scroll, and still not a copy of the whole list.
  expect(await page.getByTestId('log-row').count()).toBeLessThan(200)
})

test('a live tail streams without growing the DOM or the buffer past its cap', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('otomo.mock.tailRate', '200')
  })
  await signIn(page)
  await gotoApp(page, '/admin/dashboard/logs')

  await startTail(page)
  await page.waitForTimeout(10_000)

  const count = await lineCount(page)
  expect(count).toBeGreaterThan(500)
  expect(count).toBeLessThanOrEqual(5_000)
  expect(await page.getByTestId('log-row').count()).toBeLessThan(200)

  await page.getByTestId('tail-toggle').click()
  await expect(page.getByTestId('tail-toggle')).toHaveText(/Start/)
})

test('scrolling away from the newest lines raises the jump-to-latest pill', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('otomo.mock.tailRate', '100')
  })
  await signIn(page)
  await gotoApp(page, '/admin/dashboard/logs')

  await startTail(page)
  await expect.poll(() => lineCount(page), { timeout: 15_000 }).toBeGreaterThan(200)

  const scroll = page.getByTestId('log-scroll')
  await scroll.evaluate((element) => {
    element.scrollTop = 400
  })

  const pill = page.getByTestId('log-new-pill')
  await expect(pill).toBeVisible({ timeout: 15_000 })
  await expect(pill).toHaveText(/new line/)
  await pill.click()
  await expect(pill).toBeHidden()
})

test('Stop ends the stream, so no further tail requests are made', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('otomo.mock.tailRate', '100')
  })

  let tailRequests = 0
  page.on('request', (request) => {
    if (request.url().includes('/api/admin/dashboard/logs/tail')) tailRequests += 1
  })

  await signIn(page)
  await gotoApp(page, '/admin/dashboard/logs')

  await startTail(page)
  await page.waitForTimeout(1_500)
  expect(tailRequests).toBe(1)

  await page.getByTestId('tail-toggle').click()
  await expect(page.getByTestId('tail-toggle')).toHaveText(/Start/)

  const afterStop = tailRequests
  await page.waitForTimeout(1_500)
  expect(tailRequests).toBe(afterStop)
})
