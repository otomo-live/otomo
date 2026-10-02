import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The overview's density requirement, end to end.
 *
 * The ten-service fixture is selected with the `otomo.mock.overview` localStorage
 * flag (see the overview handler in `src/mocks/handlers.ts`). It is set before the
 * app boots with `addInitScript`, because the very first overview request decides
 * which fixture is served.
 *
 * The assertion is in bounding boxes rather than a screenshot: "fits on one
 * screen" means every card's box is inside the viewport, which a screenshot
 * comparison can only imply.
 */

test.use({ viewport: { width: 1440, height: 900 } })

async function signIn(page: Page): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill('admin@example.com')
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('ten service cards all fit on a 1440x900 viewport with the host strip', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('otomo.mock.overview', 'many')
  })
  await signIn(page)

  const cards = page.getByTestId('service-card')
  await expect(cards).toHaveCount(10)

  const viewport = page.viewportSize()
  expect(viewport).not.toBeNull()

  const boxes = await cards.evaluateAll((elements) =>
    elements.map((element) => {
      const box = element.getBoundingClientRect()
      return { top: box.top, left: box.left, right: box.right, bottom: box.bottom }
    }),
  )

  for (const box of boxes) {
    expect(box.top).toBeGreaterThanOrEqual(0)
    expect(box.left).toBeGreaterThanOrEqual(0)
    expect(box.bottom).toBeLessThanOrEqual(viewport!.height)
    expect(box.right).toBeLessThanOrEqual(viewport!.width)
  }
})

test('the default overview shows firing alerts above the cards', async ({ page }) => {
  await signIn(page)
  const panel = page.getByTestId('overview-alerts')
  await expect(panel).toBeVisible()
  await expect(panel.getByRole('heading')).toHaveText('Firing alerts (1)')
  await expect(panel.getByTestId('overview-alert')).toContainText(
    'Allocator game-server pool is empty',
  )
  await expect(panel).toHaveAttribute('role', 'status')
})
