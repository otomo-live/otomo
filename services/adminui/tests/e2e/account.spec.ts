import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The account page end to end, against the fixture API.
 *
 * The fixture's account state is real enough to assert against (a changed
 * password must actually be used to sign in again, revoking shrinks the list),
 * and enrollment accepts the fixed code `123456`, which is the only way the TOTP
 * walk completes without an authenticator.
 */

const NEW_PASSWORD = 'correct horse battery staple'

async function signIn(page: Page, email: string, password = 'x'): Promise<void> {
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}

test('a changed password is the one the next sign-in needs', async ({ page }) => {
  const email = 'viewer.changepw@example.com'
  await gotoApp(page, '/admin/login')
  await signIn(page, email)
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await gotoApp(page, '/admin/account')
  await page.getByLabel('Current password').fill('x')
  await page.getByLabel('New password').fill(NEW_PASSWORD)
  await page.getByLabel('Confirm password').fill(NEW_PASSWORD)
  await page.getByTestId('password-submit').click()
  await expect(page.getByTestId('account-toast')).toContainText('other sessions were signed out')

  await page.locator('ion-menu').getByText('Sign out').click()
  await expect(page).toHaveURL(/\/admin\/login$/)

  // The old password is refused, the new one signs in.
  await signIn(page, email)
  await expect(page.getByText('That email and password do not match an account.')).toBeVisible()
  await page.getByLabel('Password').fill(NEW_PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
})

test('signing out other sessions keeps only this device', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'liveops.revoke@example.com')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await gotoApp(page, '/admin/account')
  await expect(page.getByTestId('session-current')).toHaveCount(1)

  await page.getByTestId('revoke-others').click()
  await page.getByTestId('confirm-dialog-confirm').click()

  await expect(page.getByTestId('account-toast')).toContainText('2 other sessions')
  await expect(page.locator('.data-table__row')).toHaveCount(1)
  await expect(page.getByTestId('session-current')).toHaveCount(1)
})

test('a live_ops user enrolls a TOTP factor and sees recovery codes', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'liveops.enroll@example.com')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await gotoApp(page, '/admin/account')
  await expect(page.getByTestId('mfa-status')).toContainText('is not enabled')

  await page.getByTestId('mfa-setup').click()
  await expect(page.getByTestId('account-qr')).toBeVisible()
  await expect(page.getByTestId('account-secret')).toContainText('JBSW Y3DP EHPK 3PXP')

  await page.getByLabel('Code').fill('123456')
  await page.getByTestId('mfa-setup-confirm').click()

  await expect(page.getByTestId('recovery-codes').locator('code')).toHaveCount(10)
  await page.getByRole('checkbox', { name: 'I have saved these codes' }).focus()
  await page.keyboard.press('Space')
  await page.getByTestId('mfa-recovery-continue').click()

  await expect(page.getByTestId('mfa-status')).toContainText('is enabled')
})

test('an admin has no way to turn the required factor off', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'admin@example.com')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await gotoApp(page, '/admin/account')
  await expect(page.getByTestId('mfa-status')).toContainText('is enabled')
  await expect(page.getByTestId('mfa-admin-note')).toBeVisible()
  await expect(page.getByTestId('mfa-disable')).toHaveCount(0)
})

test('the root account is shown as password-only', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'root@example.com')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await gotoApp(page, '/admin/account')
  await expect(page.getByTestId('mfa-root-note')).toContainText('password-only')
  await expect(page.getByTestId('mfa-setup')).toHaveCount(0)
})
