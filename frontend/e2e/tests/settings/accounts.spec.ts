import { test, expect } from '@playwright/test'
import { loginAsAdmin } from '../../helpers'
import { AccountsPage } from '../../pages'

test.describe('WhatsApp Accounts - List View', () => {
  let accountsPage: AccountsPage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    accountsPage = new AccountsPage(page)
    await accountsPage.goto()
  })

  test('should display accounts page', async () => {
    await accountsPage.expectPageVisible()
    await expect(accountsPage.addButton).toBeVisible()
  })

  test('should navigate to create page when clicking Add', async ({ page }) => {
    await accountsPage.navigateToCreate()
    expect(page.url()).toContain('/settings/accounts/new')
    await expect(page.locator('h1')).toContainText('New Account')
  })

  test('should show delete confirmation from list', async ({ page }) => {
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('button').filter({ has: page.locator('svg.text-destructive') }).click()
      await expect(accountsPage.alertDialog).toBeVisible()
      await accountsPage.cancelDelete()
    }
  })

  test('should navigate to detail page when clicking account name', async ({ page }) => {
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')
      expect(page.url()).toMatch(/\/settings\/accounts\/[a-f0-9-]+$/)
    }
  })
})

test.describe('WhatsApp Accounts - Detail Page', () => {
  let accountsPage: AccountsPage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    accountsPage = new AccountsPage(page)
  })

  test('should show form fields on create page', async ({ page }) => {
    await page.goto('/settings/accounts/new')
    await page.waitForLoadState('networkidle')

    // Should have key form fields
    await expect(page.locator('input').first()).toBeVisible()
    await expect(page.locator('input[type="password"]').first()).toBeVisible()
  })

  test('should show validation error for empty required fields', async ({ page }) => {
    await page.goto('/settings/accounts/new')
    await page.waitForLoadState('networkidle')

    // Try to create without filling required fields
    await page.getByRole('button', { name: /Create/i }).click()

    // Should show validation toast
    const toast = page.locator('[data-sonner-toast]').first()
    await expect(toast).toBeVisible({ timeout: 5000 })
  })

  test('should show webhook config on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      // Should show webhook configuration card
      await expect(page.getByText('Webhook Configuration')).toBeVisible()
      await expect(page.locator('code').first()).toBeVisible()
    }
  })

  test('should have test connection button on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByRole('button', { name: /Test/i })).toBeVisible()
    }
  })

  test('should have subscribe button on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByRole('button', { name: /Subscribe/i })).toBeVisible()
    }
  })

  test('should have business profile button on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByRole('button', { name: /Profile/i })).toBeVisible()
    }
  })

  test('should show metadata on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByText('Metadata')).toBeVisible()
    }
  })

  test('should show setup guide', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByText('Setup Guide')).toBeVisible()
    }
  })

  test('should show activity log on existing account', async ({ page }) => {
    await accountsPage.goto()
    const row = page.locator('tr').filter({ has: page.locator('td') }).first()
    if (await row.isVisible()) {
      await row.locator('a').first().click()
      await page.waitForLoadState('networkidle')

      await expect(page.getByText('Activity Log')).toBeVisible()
    }
  })
})
