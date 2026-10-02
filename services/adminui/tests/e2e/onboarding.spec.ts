import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * Onboarding and TOTP enrollment end to end, against the fixture API.
 *
 * The fixture's link tokens are fixed (see `src/mocks/handlers.ts`), so these
 * cases can open a known URL. Enrollment accepts the fixed code `123456` for the
 * fixed secret, which is the only way the full walk completes without a real
 * authenticator.
 */

const STRONG_PASSWORD = 'correct horse battery staple'

async function setPassword(page: Page): Promise<void> {
  await page.getByLabel('New password').fill(STRONG_PASSWORD)
  await page.getByLabel('Confirm password').fill(STRONG_PASSWORD)
  await page.getByRole('button', { name: 'Set password' }).click()
}

test('the full invite -> enrollment -> recovery codes flow lands signed in', async ({ page }) => {
  await gotoApp(page, '/admin/onboard#token=mock-admin-invite')

  // The token is gone from the address bar before anything else is asserted.
  await expect(page).not.toHaveURL(/token=/)
  await expect(page.getByRole('heading', { name: /invited to Otomo Admin/ })).toBeVisible()

  await setPassword(page)

  // The admin invite has no TOTP factor yet, so this is enrollment.
  await expect(page).toHaveURL(/\/admin\/login\/enroll/)
  await expect(
    page.getByRole('heading', { name: 'Set up two-factor authentication' }),
  ).toBeVisible()
  await expect(page.getByAltText('TOTP enrollment QR code')).toBeVisible()
  // The secret is grouped for manual entry and has a copy control.
  await expect(page.getByTestId('enroll-secret')).toContainText('JBSW Y3DP EHPK 3PXP')
  await expect(page.getByTestId('enroll-copy-secret')).toBeVisible()

  await page.getByLabel('Code').fill('123456')
  await page.getByRole('button', { name: 'Confirm' }).click()

  // Ten recovery codes, and Continue is gated on saving them.
  await expect(page.getByTestId('recovery-codes').locator('code')).toHaveCount(10)
  const continueButton = page.getByRole('button', { name: 'Continue' })
  await expect(continueButton).toBeDisabled()

  await page.getByRole('checkbox', { name: 'I have saved these codes' }).focus()
  await page.keyboard.press('Space')
  await expect(page.getByRole('checkbox', { name: 'I have saved these codes' })).toBeChecked()
  await expect(continueButton).toBeEnabled()
  await continueButton.click()

  await expect(page).toHaveURL(/\/admin\/dashboard$/)
})

test('a live_ops invite signs straight in without a second step', async ({ page }) => {
  await gotoApp(page, '/admin/onboard#token=mock-liveops-invite')
  await expect(page.getByRole('heading', { name: /invited to Otomo Admin/ })).toBeVisible()

  await setPassword(page)

  await expect(page).toHaveURL(/\/admin\/dashboard$/)
})

test('an expired link shows the invalid-link state', async ({ page }) => {
  await gotoApp(page, '/admin/onboard#token=mock-expired')

  await expect(page.getByTestId('onboard-invalid')).toContainText('invalid or has expired')
  await expect(page).not.toHaveURL(/token=/)
  // No password form is offered for a dead link.
  await expect(page.getByLabel('New password')).toHaveCount(0)
})

test('an admin with no factor is sent to enrollment at sign-in', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill('enroll.admin@example.com')
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()

  await expect(page).toHaveURL(/\/admin\/login\/enroll/)
  await expect(page.getByAltText('TOTP enrollment QR code')).toBeVisible()
})

async function signInAsAdmin(page: Page, email: string): Promise<void> {
  await gotoApp(page, '/admin/login')
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').fill('x')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/admin\/dashboard$/)
}

/** Mints an invite through the Users page and returns the token the UI shows. */
async function inviteFromUI(page: Page, email: string, role: string): Promise<string> {
  await page.getByRole('link', { name: 'Users' }).first().click()
  await expect(page).toHaveURL(/\/admin\/users$/)
  await page.getByTestId('invite-user-open').click()
  await page.getByTestId('invite-email').fill(email)
  await page.getByTestId('invite-name').fill('Fresh Invitee')
  await page.getByTestId('invite-role').selectOption(role)
  await page.getByTestId('invite-submit').click()

  const link = await page.getByTestId('one-time-link').locator('code').textContent()
  const token = link?.split('#token=')[1] ?? ''
  expect(token).not.toBe('')
  return token
}

/**
 * The real invite path: an admin mints a link in the Users page, and the invitee
 * redeems it in a *fresh tab*. The token has to be resolvable there, which is why
 * the fixture keeps minted links in localStorage rather than per-page module
 * state (see `src/mocks/handlers.ts`).
 */
test('an invite minted in the UI redeems in a fresh page as live_ops', async ({
  page,
  context,
}) => {
  await signInAsAdmin(page, 'admin@example.com')
  const token = await inviteFromUI(page, 'fresh.liveops@example.com', 'live_ops')

  const invitee = await context.newPage()
  try {
    await gotoApp(invitee, `/admin/onboard#token=${token}`)
    await expect(invitee.getByRole('heading', { name: /invited to Otomo Admin/ })).toBeVisible()
    await setPassword(invitee)
    await expect(invitee).toHaveURL(/\/admin\/dashboard$/)
  } finally {
    await invitee.close()
  }
})

test('an admin invite minted in the UI redeems into TOTP enrollment', async ({ page, context }) => {
  // Only root may grant admin, so this is the one role that exercises the
  // enrollment branch of the invite flow through the UI.
  await signInAsAdmin(page, 'root@example.com')
  const token = await inviteFromUI(page, 'fresh.admin@example.com', 'admin')

  const invitee = await context.newPage()
  try {
    await gotoApp(invitee, `/admin/onboard#token=${token}`)
    await setPassword(invitee)

    await expect(invitee).toHaveURL(/\/admin\/login\/enroll/)
    await expect(invitee.getByAltText('TOTP enrollment QR code')).toBeVisible()
    await invitee.getByLabel('Code').fill('123456')
    await invitee.getByRole('button', { name: 'Confirm' }).click()
    await invitee.getByRole('checkbox', { name: 'I have saved these codes' }).focus()
    await invitee.keyboard.press('Space')
    await invitee.getByRole('button', { name: 'Continue' }).click()

    await expect(invitee).toHaveURL(/\/admin\/dashboard$/)
  } finally {
    await invitee.close()
  }
})
