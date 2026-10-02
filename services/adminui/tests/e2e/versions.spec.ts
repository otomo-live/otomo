import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * Version history end to end against the fixture API.
 *
 * Each test signs in, then navigates in-app or by URL within one page context, so
 * the fixture's mutable world (the draft revision, the version list) survives.
 * The conflict case uses the fixture's `__test__` draft-bump hook, which is the
 * only way to make the revision move without a second real editor. The dialog
 * cases use `ui.presentation`, whose draft differs from its latest version:
 * `balance.weapons`'s draft equals version 11, so there the dialog refuses up front.
 */

async function signIn(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('an empty message blocks version creation', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/namespaces/ui.presentation')

  await page.getByTestId('create-version').click()

  const submit = page.getByTestId('create-version-submit')
  await expect(submit).toBeDisabled()

  await page.getByTestId('create-version-message').fill('   ')
  await expect(submit).toBeDisabled()

  await page.getByTestId('create-version-message').fill('Nerf the Arc Rifle')
  await expect(submit).toBeEnabled()
})

test('versioning a draft someone else changed shows the conflict dialog', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/namespaces/ui.presentation')
  // Wait for the editor to settle before touching the fixture: a fetch fired
  // while Vite is still reloading its optimized deps never resolves.
  await expect(page.getByTestId('create-version')).toBeVisible()

  // Someone else saves the draft after this editor read the draft.
  const bumped = await page.evaluate(async () => {
    const response = await fetch(
      '/api/admin/config/__test__/namespaces/ui.presentation/draft/bump',
      { method: 'POST' },
    )
    return response.ok
  })
  expect(bumped).toBe(true)

  await page.getByTestId('create-version').click()
  await page.getByTestId('create-version-message').fill('Nerf the Arc Rifle')
  await page.getByTestId('create-version-submit').click()

  await expect(page.getByText('Someone else changed the draft')).toBeVisible()
  await expect(page.getByText('Version the newer draft')).toBeVisible()
})

test('creating a version lists it and diffs it against the previous one', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/config/namespaces/ui.presentation')

  await page.getByTestId('create-version').click()
  await page.getByTestId('create-version-message').fill('Add the render hint')
  await page.getByTestId('create-version-submit').click()
  await expect(page.getByTestId('editor-toast')).toContainText('Version 7 created')

  // In-app: the tab switch must not reload, or the fixture world is reset.
  await page.locator('ion-segment-button', { hasText: 'Versions' }).click()
  await expect(page.getByTestId('versions-table')).toBeVisible()

  await page.getByRole('row', { name: /v6/ }).click()
  await page.getByRole('row', { name: /v7/ }).click()

  await expect(page.getByTestId('diff-view')).toBeVisible()
  await expect(page.getByTestId('diff-view')).toContainText('render')
})

test('a draft identical to the latest version cannot be versioned', async ({ page }) => {
  await signIn(page, 'liveops@example.com')
  await gotoApp(page, '/admin/config/namespaces/balance.weapons')

  await page.getByTestId('create-version').click()
  await page.getByTestId('create-version-message').fill('Nothing changed')

  await expect(page.getByTestId('create-version-nothing')).toContainText('Nothing to version')
  await expect(page.getByTestId('create-version-submit')).toBeDisabled()
})
