// matrix/hardened: removing the duplicate terminal strip must preserve the
// single work-head Image control, including its native picker at every width.
import { test, expect } from '@playwright/test'

// matrix/hardened: service-worker fetches bypass page.route; block them so
// read-only settings hydration cannot race the test's explicit API response.
test.use({ serviceWorkers: 'block' })

async function selectPane(page, pane) {
  // matrix/hardened: exercise the supported navigation at each breakpoint.
  if ((page.viewportSize()?.width || 1280) < 768) {
    await page.locator(`[data-testid="mobile-tab-${pane}"]`).click()
  } else {
    await page.locator('.top-tab', { hasText: pane === 'terminal' ? 'Terminal' : 'Costs' }).click()
  }
}

async function expectImagePicker(page) {
  // matrix/hardened: a visible button alone cannot prove the moved input is
  // still registered; observe the browser picker without uploading a file.
  const picker = page.waitForEvent('filechooser')
  await page.locator('.work-head .wh-image').click()
  const chooser = await picker
  expect(await chooser.element().getAttribute('type')).toBe('file')
  expect(await chooser.element().getAttribute('accept')).toContain('image/png')
}

test.describe('terminal uses a single session header', () => {
  test.beforeEach(async ({ request }) => {
    const reset = await request.post('/__fixture/reset')
    expect(reset.status()).toBe(204)
  })

  test('Image opens its picker before and after switching panes at every viewport', async ({ page }) => {
    await page.goto('/s/sess-001')
    await expect(page.locator('.xterm-screen')).toBeVisible()
    await expect(page.locator('.work-head .cur')).toHaveText('agent-deck')
    await expect(page.locator('.term-strip')).toHaveCount(0)
    await expect(page.locator('.work-head .wh-image')).toBeVisible()
    await expectImagePicker(page)

    await selectPane(page, 'costs')
    await expect(page.locator('.chart-card .title', { hasText: 'Cost tracking unavailable' })).toBeVisible()
    await expect(page.locator('.work-head .wh-image')).toHaveCount(0)

    await selectPane(page, 'terminal')
    await expect(page.locator('.xterm-screen')).toBeVisible()
    await expect(page.locator('.work-head .wh-image')).toBeVisible()
    await expectImagePicker(page)
  })

  test('read-only settings keep the Image control and session actions absent', async ({ page }) => {
    // matrix/hardened: use the app's actual settings hydration path to enter
    // read-only mode; this contract needs no second fixture or server writes.
    await page.route('**/api/settings', async route => {
      const response = await route.fetch()
      const settings = await response.json()
      await route.fulfill({ response, json: { ...settings, webMutations: false } })
    })

    await page.goto('/s/sess-001')
    await expect(page.locator('.xterm-screen')).toBeVisible()
    await expect(page.locator('.work-head .cur')).toHaveText('agent-deck')
    await expect(page.locator('.term-strip')).toHaveCount(0)
    await expect(page.locator('.work-head .actions')).toHaveCount(0)
    await expect(page.locator('.work-head .wh-image')).toHaveCount(0)
  })
})
