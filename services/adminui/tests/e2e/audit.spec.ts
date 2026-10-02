import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The audit's release link, end to end.
 *
 * The fixture's merged audit carries a `release.publish` row for `dev` release 3
 * with base 2, and the releases fixture holds both manifests, so the whole path
 * is exercised in a browser: the audit renders the link, the diff page reads the
 * two releases and shows the namespace version move, and the browser's back
 * button returns to the filtered audit URL.
 */

async function signIn(page: Page): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill('admin@example.com')
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('a publish row opens the matching diff and back keeps the filters', async ({ page }) => {
  await signIn(page)
  await gotoApp(page, '/admin/dashboard/audit')

  // A filter in the URL is the thing the back button has to preserve.
  await page.getByTestId('audit-filter-source').selectOption('config')
  await expect(page).toHaveURL(/source=config/)

  const publishRow = page.locator('tr', { hasText: 'release.publish' }).first()
  await expect(publishRow).toBeVisible()
  await publishRow.getByTestId('audit-changes-link').click()

  await expect(page).toHaveURL(/\/admin\/config\/releases\/dev\/3\?base=2$/)
  await expect(page.getByTestId('release-diff-page')).toBeVisible()

  // The release's own header and the namespace it moved: 10 to 11.
  await expect(page.getByRole('link', { name: 'balance.weapons' })).toBeVisible()
  await expect(page.getByText('10 → 11')).toBeVisible()

  await page.goBack()
  await expect(page).toHaveURL(/\/admin\/dashboard\/audit\?source=config$/)
  await expect(page.getByTestId('audit-filter-source')).toHaveValue('config')
})
