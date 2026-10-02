import { expect, gotoApp, test, type Page } from './fixtures'

/**
 * The sign-in path end to end, in a real browser against the fixture API.
 *
 * The fixture derives roles from the email's local part (see
 * `src/mocks/handlers.ts`), which is what lets one suite cover the three
 * accounts: `admin@` everything, `mfa.ops@` a second factor, `viewer@` the
 * dashboard alone.
 *
 * One thing to know before reading the URL assertions: vue-router does not
 * percent-encode `/` inside a query value, so a carried path is `returnTo=/config`
 * in the address bar rather than the `%2Fconfig` everyone expects to see.
 */

/**
 * Fields are found by their label, which works because the inputs carry Ionic's
 * own `label` prop. Sibling IonLabels look identical and associate nothing, so
 * a selector built on those would have to reach for the DOM structure instead,
 * and would keep passing after the field lost its accessible name.
 */
function field(page: Page, label: string) {
  return page.getByLabel(label)
}

async function signIn(page: Page, email: string, password: string): Promise<void> {
  await field(page, 'Email').fill(email)
  await field(page, 'Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}

test('an anonymous deep link signs in and continues to where it was going', async ({ page }) => {
  await gotoApp(page, '/admin/config')

  // The guard kept the destination in the query rather than dropping it.
  await expect(page).toHaveURL(/\/admin\/login\?returnTo=\/config$/)
  await expect(page.getByRole('heading', { name: 'Otomo Admin' })).toBeVisible()

  await signIn(page, 'admin@example.com', 'correct horse battery staple')

  await expect(page).toHaveURL(/\/admin\/config$/)
  // A namespace from the fixture, so this is the list read reaching the API and
  // rendering what came back, rather than a static string on the page.
  await expect(page.getByRole('link', { name: 'balance.weapons' })).toBeVisible()
})

test('the second factor is asked for and then honoured', async ({ page }) => {
  await gotoApp(page, '/admin/login')

  await signIn(page, 'mfa.ops@example.com', 'x')

  // No session yet: the password was accepted, the challenge was not answered.
  await expect(page).toHaveURL(/\/admin\/login\/mfa/)
  await expect(page.getByRole('heading', { name: 'Second factor' })).toBeVisible()

  await field(page, 'Code').fill('123456')
  await page.getByRole('button', { name: 'Verify' }).click()

  await expect(page).toHaveURL(/\/admin\/dashboard$/)
})

test('a refused sign-in is explained without leaving the form', async ({ page }) => {
  await gotoApp(page, '/admin/login')

  await signIn(page, 'denied@example.com', 'x')

  await expect(page).toHaveURL(/\/admin\/login$/)
  await expect(page.getByText('That email and password do not match an account.')).toBeVisible()
  // The request id, which is the only part of a failure worth quoting to
  // whoever administers the service.
  // Unanchored on purpose: the id sits inside an IonNote whose slotted text
  // keeps the surrounding whitespace from the template, so a `^`-anchored match
  // finds nothing.
  await expect(page.getByText(/Reference req_/)).toBeVisible()
  // And the password does not stay in the field.
  await expect(field(page, 'Password')).toHaveValue('')
})

test('the navigation shows what the role reaches, and nothing more', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'viewer@example.com', 'x')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  const nav = page.locator('ion-menu')
  await expect(nav.getByText('Overview')).toBeVisible()
  await expect(nav.getByText('Logs')).toBeVisible()
  // GET /namespaces is a viewer read, so the namespace list is reachable.
  await expect(nav.getByText('Namespaces')).toBeVisible()

  await nav.getByText('Namespaces').click()
  await expect(page).toHaveURL(/\/admin\/config$/)

  // The editor opens to viewers now, but read-only: its mutating controls are
  // gone while the route and the reads remain.
  await gotoApp(page, '/admin/config/namespaces/balance.weapons')
  await expect(page).toHaveURL(/\/admin\/config\/namespaces\/balance\.weapons$/)
  await expect(page.getByTestId('save-draft')).toHaveCount(0)
  await expect(page.getByTestId('create-version')).toHaveCount(0)
})

test('an admin reaches both areas, and signs out', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'admin@example.com', 'x')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  const nav = page.locator('ion-menu')
  await expect(nav.getByText('Namespaces')).toBeVisible()

  await nav.getByText('Namespaces').click()
  await expect(page).toHaveURL(/\/admin\/config$/)

  // The shell names the account and offers the way out. The email rather than
  // the display name: the sidebar also has an "Admin" group
  // label, so the name is no longer unique in the menu.
  await expect(nav.getByText('admin@example.com')).toBeVisible()
  await nav.getByText('Sign out').click()
  await expect(page).toHaveURL(/\/admin\/login$/)

  // Signed out means signed out: the deep link is refused again.
  await gotoApp(page, '/admin/dashboard')
  await expect(page).toHaveURL(/\/admin\/login\?returnTo=\/dashboard$/)
})

test('the route palette jumps to a page with Ctrl+K', async ({ page }) => {
  await gotoApp(page, '/admin/login')
  await signIn(page, 'admin@example.com', 'x')
  await expect(page).toHaveURL(/\/admin\/dashboard$/)

  await page.keyboard.press('Control+k')
  // Typed straight after the shortcut, with no click: the search box has focus.
  const search = page.getByRole('combobox', { name: 'Search pages' })
  await expect(search).toBeFocused()
  await page.keyboard.type('logs')
  await page.keyboard.press('Enter')

  await expect(page).toHaveURL(/\/admin\/dashboard\/logs$/)
})
