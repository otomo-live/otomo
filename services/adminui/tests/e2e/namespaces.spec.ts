import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * Namespace create and schema replace, end to end against the fixture API.
 *
 * Both flows are SPA navigations on purpose: the fixture's namespace state lives
 * in the page (the MSW worker forwards each request back to the page's handlers),
 * so a full reload would reset it and prove nothing. The list is reached through
 * in-app links for the same reason.
 */

async function signIn(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('an admin creates a namespace, versions its schema, and sees it listed', async ({ page }) => {
  await signIn(page, 'admin@example.com')

  await gotoApp(page, '/admin/config')
  await page.getByTestId('create-namespace').click()

  await page.getByTestId('create-namespace-name').fill('test.e2e_ns')
  await page.getByTestId('create-namespace-audience-client').check()
  await page.getByTestId('create-namespace-description').fill('Created by the end-to-end test.')
  await page.getByTestId('create-namespace-submit').click()

  // Success lands on the new namespace's schema page, which starts at v1.
  await expect(page).toHaveURL(/\/admin\/config\/namespaces\/test\.e2e_ns\/schema$/)
  await expect(page.getByText('schema v1')).toBeVisible()

  await page.getByTestId('schema-edit').click()
  const editor = page.getByRole('textbox', { name: 'test.e2e_ns schema as JSON' })
  await editor.fill('{"type":"object"}')
  await page.getByTestId('schema-save').click()

  await expect(page.getByTestId('schema-toast')).toContainText('Schema v2 saved')

  // In-app navigation, so the fixture keeps the namespace it just created.
  await page.getByRole('link', { name: 'Namespaces' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config$/)
  await expect(page.getByRole('cell', { name: /test\.e2e_ns/ })).toBeVisible()
  await expect(page.getByRole('link', { name: 'balance.weapons' })).toBeVisible()
})

test('a viewer sees the list but no Create, and the editor read-only', async ({ page }) => {
  await signIn(page, 'viewer@example.com')

  await gotoApp(page, '/admin/config')
  await expect(page.getByRole('heading', { name: 'Namespaces' })).toBeVisible()
  await expect(page.getByTestId('create-namespace')).toHaveCount(0)

  // Names link into the editor for everyone now; a viewer gets it read-only.
  await page.getByRole('link', { name: 'balance.weapons' }).first().click()
  await expect(page).toHaveURL(/\/admin\/config\/namespaces\/balance\.weapons$/)
  await expect(page.getByTestId('save-draft')).toHaveCount(0)
  await expect(page.getByTestId('create-version')).toHaveCount(0)
  await expect(page.getByRole('spinbutton', { name: 'Damage' })).toBeDisabled()

  await gotoApp(page, '/admin/config/namespaces/balance.weapons/schema')
  await expect(page).toHaveURL(/\/admin\/config\/namespaces\/balance\.weapons\/schema$/)
  await expect(page.getByTestId('schema-edit')).toHaveCount(0)
})
