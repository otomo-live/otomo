import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The release history end to end against the fixture API.
 *
 * The fixture's dev head is release 3 and its live head is release 2, which has
 * an earlier release 1 to roll back to. A rollback moves the head pointer
 * without writing a row, so the assertion that matters is that the head card and
 * the history's head mark move together immediately.
 */

async function signIn(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('an admin rolls dev back and the head and history update immediately', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/config/releases?channel=dev')

  await expect(page.getByTestId('head-release-id')).toHaveText('3')
  // The first non-head row is release 2.
  await page.getByTestId('rollback-release').first().click()
  await expect(page.getByTestId('rollback-summary')).toContainText('back to release 2')
  await page.getByTestId('rollback-confirm').click()

  await expect(page.getByTestId('release-toast')).toContainText('Rolled back dev to release 2')
  await expect(page.getByTestId('head-release-id')).toHaveText('2')

  const headRow = page.locator('tr', { has: page.getByTestId('release-head-badge') })
  await expect(headRow).toContainText('2')
})

test('an admin promotes dev to staging with a message', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/config/releases?channel=staging')

  await expect(page.getByTestId('head-release-id')).toHaveText('1')
  await page.getByTestId('promote-release').click()
  await expect(page.getByTestId('promote-summary')).toContainText('Promote release 3 of dev')
  await page.getByTestId('promote-message').fill('Promote the balance pass')
  await page.getByTestId('promote-confirm').click()

  await expect(page.getByTestId('release-toast')).toContainText('Promoted dev release 3 to staging')
  await expect(page.getByTestId('head-release-id')).toHaveText('4')
})

test('a live rollback needs the word live typed', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/config/releases?channel=live')

  await expect(page.getByTestId('head-release-id')).toHaveText('2')
  await page.getByTestId('rollback-release').first().click()
  await expect(page.getByTestId('rollback-live-gate')).toBeVisible()
  await expect(page.getByTestId('rollback-confirm')).toBeDisabled()

  await page.getByTestId('rollback-live-confirm').fill('live')
  await expect(page.getByTestId('rollback-confirm')).toBeEnabled()
  await page.getByTestId('rollback-confirm').click()

  await expect(page.getByTestId('release-toast')).toContainText('Rolled back live to release 1')
  await expect(page.getByTestId('head-release-id')).toHaveText('1')
})

test('a live_ops user sees neither rollback nor promote', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/releases?channel=live')

  await expect(page.getByTestId('head-release-id')).toHaveText('2')
  await expect(page.getByTestId('rollback-release')).toHaveCount(0)
  await expect(page.getByTestId('promote-release')).toHaveCount(0)
})
