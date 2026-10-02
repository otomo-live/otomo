import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The service detail page, end to end: the overview links into it, the range
 * lives in the URL, and a 7d chart of 1500 points still paints fast.
 *
 * The fixture API supplies the series data. The performance case overrides it
 * with a deliberately maximal answer (1500 points, four series) so the budget is
 * measured against the worst shape the endpoint can return, not a small sample.
 */

async function signIn(page: Page): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill('admin@example.com')
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

function cardFor(page: Page, name: string) {
  return page.locator('[data-testid="service-card"]', { hasText: name }).first()
}

test('an overview card opens the service detail page', async ({ page }) => {
  await signIn(page)
  await cardFor(page, 'config').click()

  await expect(page).toHaveURL(/\/admin\/dashboard\/services\/config$/)
  // Ionic keeps the previous page in the view stack, so scope to the name.
  await expect(page.getByRole('heading', { level: 1, name: 'config' })).toHaveText('config')
  await expect(page.getByTestId('range-1h')).toHaveAttribute('data-selected', 'true')
  // The breadcrumb walks back to the overview.
  await expect(page.locator('.breadcrumbs__link', { hasText: 'Overview' }).first()).toBeVisible()
})

test('the range is URL state and survives a reload', async ({ page }) => {
  await signIn(page)
  await gotoApp(page, '/admin/dashboard/services/config?range=1h')

  await page.getByTestId('range-24h').click()
  await expect(page).toHaveURL(/range=24h/)

  await page.reload()
  await expect(page.getByTestId('range-24h')).toHaveAttribute('data-selected', 'true')
  await expect(page.getByTestId('range-1h')).toHaveAttribute('data-selected', 'false')
  await expect(page.getByTestId('time-series-chart').first()).toBeVisible()
})

test('a 1500 x 4 chart renders its first draw in under 500 ms', async ({ page }) => {
  await page.route('**/api/admin/dashboard/services/*/series*', async (route) => {
    const metric = new URL(route.request().url()).searchParams.get('metric') ?? 'series'
    await route.fulfill({ json: maximalSeries(metric) })
  })

  await signIn(page)

  // A first, fully rendered pass so the measured draw is a redraw of a live
  // chart with the heavy data, not the page's cold start.
  await gotoApp(page, '/admin/dashboard/services/config?range=1h')
  await page.waitForFunction(
    () => document.querySelectorAll('[data-testid="time-series-chart"][data-rendered]').length >= 8,
  )

  // The clock starts in the page, on the same tick as the click: Playwright's
  // `click` spends a few hundred milliseconds on actionability checks and CDP
  // round-trips, and none of that is the chart's draw.
  const start = await page.evaluate(() => {
    const button = document.querySelector<HTMLElement>('[data-testid="range-7d"]')
    if (button === null) throw new Error('range-7d button not found')
    const t0 = performance.now()
    button.click()
    return t0
  })

  // `data-rendered` is the draw's timestamp, so waiting for one at or after
  // `start` waits for the redraw this test is measuring.
  await page.waitForFunction((t0: number) => {
    const chart = document.querySelector('[data-testid="time-series-chart"]')
    const rendered = chart?.getAttribute('data-rendered')
    return rendered !== null && rendered !== undefined && Number(rendered) >= t0
  }, start)

  const rendered = await page.evaluate(() =>
    Number(
      document.querySelector('[data-testid="time-series-chart"]')?.getAttribute('data-rendered'),
    ),
  )
  expect(rendered - start).toBeLessThan(500)
})

/** The largest body DSH-C6 allows: 1500 points across four series. */
function maximalSeries(metric: string) {
  const now = Math.floor(Date.now() / 1000)
  const t = Array.from({ length: 1500 }, (_value, index) => now - (1499 - index) * 60)
  const series = Array.from({ length: 4 }, (_value, seriesIndex) => ({
    name: `s${seriesIndex + 1}`,
    values: t.map((_point, index) =>
      index % 97 === 0 ? null : 10 + seriesIndex + Math.sin(index / 20) * 5,
    ),
  }))
  return {
    metric,
    title: metric,
    unit: 'ms',
    service: 'config',
    from: t[0],
    to: t[t.length - 1],
    step: 60,
    clamped: false,
    t,
    series,
  }
}
