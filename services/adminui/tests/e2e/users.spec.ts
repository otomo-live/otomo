import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * Staff user management end to end, against the fixture API.
 *
 * The fixture seeds a `root@example.com` identity (with `is_root` true) because
 * the admin fixture had none, and D3's "granting admin requires root" has no
 * other way to be exercised in the browser.
 */

async function signIn(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

test('a live_ops user never sees Users, in the menu or at the URL', async ({ page }) => {
  await signIn(page, 'liveops@example.com')

  const nav = page.locator('ion-menu')
  await expect(nav.getByText('Overview')).toBeVisible()
  await expect(nav.getByText('Users')).toHaveCount(0)

  await gotoApp(page, '/admin/users')
  await expect(page).toHaveURL(/\/admin\/denied/)
  await expect(page.getByTestId('users-page')).toHaveCount(0)
})

test('an admin invites a live_ops user, copies the link, then revokes it', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/users')

  await page.getByTestId('invite-user-open').click()
  await page.getByTestId('invite-email').fill('newliveops@example.com')
  await page.getByTestId('invite-name').fill('New Live Ops')
  await page.getByTestId('invite-role').selectOption('live_ops')
  await page.getByTestId('invite-submit').click()

  const oneTime = page.getByTestId('one-time-link')
  await expect(oneTime).toBeVisible()
  await expect(oneTime).toContainText('http://localhost:8090/admin/onboard#token=')
  await expect(page.getByTestId('one-time-link-warning')).toContainText('shown once')

  // The copy button writes the whole URL; the link remains readable either way.
  await page.getByRole('button', { name: 'Copy one-time link' }).click()

  await page.getByTestId('one-time-link-close').click()
  await expect(page.getByTestId('one-time-link')).toHaveCount(0)

  // The link is gone from the page, but the pending invite is listed.
  await expect(page.getByTestId('users-page').getByText('newliveops@example.com')).toBeVisible()
  await expect(page.getByTestId('users-page')).not.toContainText('token=')

  await page.getByTestId('revoke-invite').first().click()
  await expect(page.getByTestId('confirm-dialog-message')).toContainText('newliveops@example.com')
  await page.getByTestId('confirm-dialog-confirm').click()
  await expect(page.getByTestId('revoke-invite')).toHaveCount(0)
})

test('an admin cannot pick admin when inviting', async ({ page }) => {
  await signIn(page, 'admin@example.com')
  await gotoApp(page, '/admin/users')
  await page.getByTestId('invite-user-open').click()

  const options = await page.getByTestId('invite-role').locator('option').allTextContents()
  expect(options).toEqual(['viewer', 'live_ops'])
})

test('root can pick admin, and the root row is marked', async ({ page }) => {
  await signIn(page, 'root@example.com')
  await gotoApp(page, '/admin/users')

  await expect(page.getByTestId('user-root-badge')).toBeVisible()
  await page.getByTestId('invite-user-open').click()

  const options = await page.getByTestId('invite-role').locator('option').allTextContents()
  expect(options).toEqual(['viewer', 'live_ops', 'admin'])
})
