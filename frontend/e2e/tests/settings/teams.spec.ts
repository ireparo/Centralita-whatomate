import { test, expect } from '@playwright/test'
import { TablePage } from '../../pages'
import { loginAsAdmin, createTeamFixture } from '../../helpers'

test.describe('Teams Management', () => {
  let tablePage: TablePage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    tablePage = new TablePage(page)
  })

  test('should display teams list', async ({ page }) => {
    await expect(tablePage.tableBody).toBeVisible()
  })

  test('should search teams', async ({ page }) => {
    const initialCount = await tablePage.getRowCount()
    if (initialCount > 0) {
      const firstRow = tablePage.tableRows.first()
      const teamName = await firstRow.locator('td').first().textContent()
      if (teamName) {
        await tablePage.search(teamName.trim())
        await page.waitForTimeout(300)
        const filteredCount = await tablePage.getRowCount()
        expect(filteredCount).toBeLessThanOrEqual(initialCount)
      }
    }
  })

  test('should navigate to create team page', async ({ page }) => {
    await tablePage.clickAddButton()
    await page.waitForLoadState('networkidle')
    expect(page.url()).toContain('/settings/teams/new')
    await expect(page.locator('h1')).toContainText('New Team')
  })

  test('should create a new team', async ({ page }) => {
    const newTeam = createTeamFixture()

    // Navigate to create page
    await tablePage.clickAddButton()
    await page.waitForLoadState('networkidle')

    // Fill form
    await page.locator('input').first().fill(newTeam.name)
    await page.locator('textarea').first().fill(newTeam.description)

    // Save
    await page.getByRole('button', { name: /Create/i }).click()
    await page.waitForLoadState('networkidle')

    // Should redirect to detail page
    expect(page.url()).toContain('/settings/teams/')
    expect(page.url()).not.toContain('/new')

    // Go back to list and verify
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    await tablePage.search(newTeam.name)
    await tablePage.expectRowExists(newTeam.name)
  })

  test('should edit existing team', async ({ page }) => {
    // Create a team first via the detail page
    const team = createTeamFixture()

    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')
    await page.locator('input').first().fill(team.name)
    await page.getByRole('button', { name: /Create/i }).click()
    await page.waitForLoadState('networkidle')

    // Now edit - update the name
    const updatedName = team.name + ' Updated'
    await page.locator('input').first().fill(updatedName)
    await page.getByRole('button', { name: /Save/i }).click()
    await page.waitForTimeout(1000)

    // Go back to list and verify
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    await tablePage.search(updatedName)
    await tablePage.expectRowExists(updatedName)
  })

  test('should navigate to detail page when clicking team name', async ({ page }) => {
    const rowCount = await tablePage.getRowCount()
    if (rowCount > 0) {
      const firstRow = tablePage.tableRows.first()
      const link = firstRow.locator('a').first()
      await link.click()
      await page.waitForLoadState('networkidle')
      expect(page.url()).toMatch(/\/settings\/teams\/[a-f0-9-]+$/)
    }
  })

  test('should delete team', async ({ page }) => {
    // Create a team to delete
    const team = createTeamFixture({ name: 'Team To Delete ' + Date.now() })

    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')
    await page.locator('input').first().fill(team.name)
    await page.getByRole('button', { name: /Create/i }).click()
    await page.waitForLoadState('networkidle')

    // Go back to list
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')

    // Delete the team
    await tablePage.search(team.name)
    await tablePage.deleteRow(team.name)

    // Verify deletion
    await tablePage.clearSearch()
    await tablePage.search(team.name)
    await tablePage.expectRowNotExists(team.name)
  })
})

test.describe('Teams - Table Sorting', () => {
  let tablePage: TablePage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    tablePage = new TablePage(page)
  })

  test('should sort by team name', async () => {
    await tablePage.clickColumnHeader('Team')
    const direction = await tablePage.getSortDirection('Team')
    expect(direction).not.toBeNull()
  })

  test('should sort by strategy', async () => {
    await tablePage.clickColumnHeader('Strategy')
    const direction = await tablePage.getSortDirection('Strategy')
    expect(direction).not.toBeNull()
  })

  test('should sort by status', async () => {
    await tablePage.clickColumnHeader('Status')
    const direction = await tablePage.getSortDirection('Status')
    expect(direction).not.toBeNull()
  })

  test('should sort by created date', async () => {
    await tablePage.clickColumnHeader('Created')
    const direction = await tablePage.getSortDirection('Created')
    expect(direction).not.toBeNull()
  })

  test('should toggle sort direction', async () => {
    await tablePage.clickColumnHeader('Team')
    const firstDirection = await tablePage.getSortDirection('Team')

    await tablePage.clickColumnHeader('Team')
    const secondDirection = await tablePage.getSortDirection('Team')

    expect(firstDirection).not.toEqual(secondDirection)
  })
})

test.describe('Team Members', () => {
  test('should add member on detail page', async ({ page }) => {
    await loginAsAdmin(page)

    // Create a team
    const team = createTeamFixture()
    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')
    await page.locator('input').first().fill(team.name)
    await page.getByRole('button', { name: /Create/i }).click()
    await page.waitForLoadState('networkidle')

    // Should be on detail page with Members section
    await expect(page.getByText('Members')).toBeVisible()

    // Find add member button
    const addAsAgentButton = page.getByRole('button', { name: /Agent/i }).first()

    if (await addAsAgentButton.isVisible()) {
      await addAsAgentButton.click()
      await page.waitForTimeout(500)

      // Verify member appears in the members list
      const memberItems = page.locator('.flex.items-center.gap-3').filter({ has: page.locator('text=agent') })
      const count = await memberItems.count()
      expect(count).toBeGreaterThan(0)
    }
  })

  test('should remove member on detail page', async ({ page }) => {
    await loginAsAdmin(page)

    // Create a team and add a member
    const team = createTeamFixture()
    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')
    await page.locator('input').first().fill(team.name)
    await page.getByRole('button', { name: /Create/i }).click()
    await page.waitForLoadState('networkidle')

    // Add a member
    const addButton = page.getByRole('button', { name: /Agent/i }).first()
    if (await addButton.isVisible()) {
      await addButton.click()
      await page.waitForTimeout(500)

      // Remove the member
      const removeButton = page.locator('button').filter({ has: page.locator('svg') }).filter({ hasText: '' }).last()
      if (await removeButton.isVisible()) {
        await removeButton.click()
        // Confirm removal
        const confirmButton = page.getByRole('button', { name: /Remove/i })
        if (await confirmButton.isVisible()) {
          await confirmButton.click()
        }
      }
    }
  })
})
