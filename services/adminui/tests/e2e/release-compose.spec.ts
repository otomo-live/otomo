import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The release composer end to end against the fixture API.
 *
 * The fixture's dev head is release 3; a publish appends release 4 and moves the
 * head, so the diff page the composer lands on compares 4 against 3. The stale
 * case moves the head through the fixture's `__test__` bump hook, the only way to
 * make the channel move without a second composer. Live is admin-only and needs
 * the word typed, and the page refuses a live_ops session by URL as well.
 */

async function signIn(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('publishes to dev as live_ops and lands on the release diff', async ({ page }) => {
  await signIn(page, 'liveops@example.com')

  // In-app, so the fixture's channel state and the session survive.
  await page.getByRole('link', { name: 'Namespaces' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config$/)
  await expect(page.getByTestId('compose-release')).toBeVisible()
  await page.getByTestId('compose-release').click()
  await expect(page).toHaveURL(/\/admin\/config\/releases\/dev\/compose$/)

  // Move balance.weapons from the head's v11 to v10 so the preview has a change.
  await page.getByTestId('compose-version-balance.weapons').selectOption('10')
  await page.getByTestId('compose-message').fill('Roll balance.weapons back to v10')
  await page.getByTestId('compose-publish').click()

  await expect(page).toHaveURL(/\/admin\/config\/releases\/dev\/4$/)
  await expect(page.getByTestId('release-diff-page')).toBeVisible()
  // Scoped: Ionic keeps the composer view in the outlet, so "11 → 10" is on
  // screen twice.
  await expect(page.getByTestId('release-diff-page').getByText('11 → 10')).toBeVisible()
})

test('live publishing needs the word live typed, for an admin', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/config/releases/live/compose')

  await expect(page.getByTestId('compose-publish')).toBeVisible()
  await page.getByTestId('compose-message').fill('Ship the balance pass')
  await page.getByTestId('compose-version-balance.weapons').selectOption('10')

  await expect(page.getByTestId('compose-publish')).toBeDisabled()
  await page.getByTestId('compose-live-confirm').fill('live')
  await expect(page.getByTestId('compose-publish')).toBeEnabled()
})

test('a live_ops user is refused live composition', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/releases/live/compose')

  await expect(page.getByTestId('compose-live-denied')).toContainText('administrators')
  await expect(page.getByTestId('compose-publish')).toHaveCount(0)
})

test('a head that moved under the composer opens the stale dialog', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/releases/dev/compose')

  await page.getByTestId('compose-message').fill('Roll balance.weapons back to v10')
  await page.getByTestId('compose-version-balance.weapons').selectOption('10')

  // Someone else publishes to dev after this composer read its head.
  const bumped = await page.evaluate(async () => {
    const response = await fetch('/api/admin/config/__test__/channels/dev/head/bump', {
      method: 'POST',
    })
    return response.ok
  })
  expect(bumped).toBe(true)

  await page.getByTestId('compose-publish').click()

  await expect(page.getByText('Someone published to dev since you opened this')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Review the new head' })).toBeVisible()
})

/**
 * The whole content pipeline in one page context: edit a draft, save it, cut a
 * version, compose a dev release around that version, publish it, then roll the
 * channel back and watch history follow.
 *
 * Every step after sign-in is an in-app navigation on purpose. The fixture's
 * mutable world (the draft revision, the version list, the channel head) lives in
 * the page's copy of the handlers, so a `page.goto` would reset it between the
 * editor and the composer and the version would not be there to select.
 */
test('the draft -> version -> publish -> rollback flow completes', async ({ page }) => {
  await signIn(page, 'admin@example.com')

  await page.getByRole('link', { name: 'Namespaces' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config$/)
  await page.getByRole('link', { name: 'balance.weapons' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config\/namespaces\/balance\.weapons$/)

  // 1. Edit the draft in the raw tab: bump Damage from 40 to 45 and save.
  await page.locator('ion-segment-button', { hasText: 'JSON' }).click()
  const document = page.getByRole('textbox', { name: 'balance.weapons document as JSON' })
  await document.fill('{"name":"Arc Rifle","damage":45,"range":18.5,"ammo":6}')
  await expect(page.getByTestId('status-unsaved')).toContainText('Unsaved changes')
  await page.getByTestId('save-draft').click()
  await expect(page.getByTestId('status-revision')).toHaveText('draft rev 13')

  // 2. Cut version 12 from the saved draft.
  await page.getByTestId('create-version').click()
  await page.getByTestId('create-version-message').fill('Raise the Arc Rifle damage')
  await page.getByTestId('create-version-submit').click()
  await expect(page.getByTestId('editor-toast')).toContainText('Version 12 created')

  // 3. Compose a dev release carrying v12 and publish it.
  await page.getByRole('link', { name: 'Namespaces' }).first().click()
  await page.getByTestId('compose-release').click()
  await expect(page).toHaveURL(/\/admin\/config\/releases\/dev\/compose$/)
  await page.getByTestId('compose-version-balance.weapons').selectOption('12')
  await page.getByTestId('compose-message').fill('Ship the raised damage')
  await page.getByTestId('compose-publish').click()
  await expect(page).toHaveURL(/\/admin\/config\/releases\/dev\/4$/)
  await expect(page.getByTestId('release-diff-page')).toBeVisible()

  // 4. The new release is the head in history.
  await page.getByRole('link', { name: 'Releases' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config\/releases(\?channel=dev)?$/)
  await expect(page.getByTestId('head-release-id')).toHaveText('4')
  await expect(page.getByRole('cell', { name: 'Ship the raised damage' })).toBeVisible()

  // 5. Roll back to release 3 and see the head and the history move together.
  await page.getByTestId('rollback-release').first().click()
  await page.getByTestId('rollback-confirm').click()
  await expect(page.getByTestId('release-toast')).toContainText('Rolled back dev to release 3')
  await expect(page.getByTestId('head-release-id')).toHaveText('3')
  const headRow = page.locator('tr', { has: page.getByTestId('release-head-badge') })
  await expect(headRow).toContainText('3')
})
