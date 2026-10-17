// matrix/hardened: dashboard recovery uses one native push-only WebSocket and
// short JSON probes through the production service worker.
import { test, expect } from '@playwright/test'
import { expandSeededCollapsedGroups } from '../helpers/seededSidebar.js'

const RECOVERED_TITLE = 'recovered-after-offline'
const OLD_SERVICE_WORKER = `
self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', event => event.waitUntil(self.clients.claim()))
`

async function observeDashboard(page) {
  await page.addInitScript(() => {
    const NativeWebSocket = window.WebSocket
    const nativeFetch = window.fetch.bind(window)
    window.__agentDeckDashboard = []
    window.__agentDeckMenuProbeStatuses = []
    window.fetch = async (...args) => {
      const response = await nativeFetch(...args)
      const rawURL = typeof args[0] === 'string' ? args[0] : args[0]?.url
      if (rawURL && new URL(rawURL, location.href).pathname === '/api/menu') {
        window.__agentDeckMenuProbeStatuses.push(response.status)
      }
      return response
    }
    window.WebSocket = new Proxy(NativeWebSocket, {
      construct(Target, args) {
        const source = new Target(...args)
        if (new URL(args[0], location.href).pathname === '/ws/dashboard') {
          const state = { source, errors: 0 }
          source.addEventListener('error', () => { state.errors += 1 })
          window.__agentDeckDashboard.push(state)
        }
        return source
      },
    })
  })
}
const dashboardCount = page => page.evaluate(() => window.__agentDeckDashboard.length)
const statusCount = (page, status) => page.evaluate(status =>
  window.__agentDeckMenuProbeStatuses.filter(value => value === status).length, status,
)
const waitForOpen = (page, count) => page.waitForFunction(count =>
  window.__agentDeckDashboard.length === count &&
  window.__agentDeckDashboard.at(-1).source.readyState === WebSocket.OPEN, count,
)

async function showCommandCenter(page) {
  if ((page.viewportSize()?.width || 1280) < 768) {
    await page.locator('[data-testid="mobile-tab-command-center"]').click()
  } else {
    await page.locator('.top-tab', { hasText: 'Command Center' }).click()
  }
}

test.describe('service worker dashboard recovery', () => {
  test.beforeEach(async ({ request, page }) => {
    expect((await request.post('/__fixture/reset')).status()).toBe(204)
    expect((await request.post('/__fixture/stream-outage?enabled=false')).status()).toBe(200)
    await observeDashboard(page)
  })

  test('the existing page recovers both snapshots over one socket after a transient outage', async ({ context, page, request }) => {
    let serveOldWorker = true
    await context.route('**/sw.js', async route => {
      if (serveOldWorker) {
        await route.fulfill({
          status: 200, contentType: 'application/javascript; charset=utf-8',
          headers: { 'Cache-Control': 'no-store', 'Service-Worker-Allowed': '/' },
          body: OLD_SERVICE_WORKER,
        })
      } else {
        await route.continue()
      }
    })
    await expandSeededCollapsedGroups(page)
    await page.goto('/')
    await expect(page.locator('.sess')).toHaveCount(4, { timeout: 5000 })
    await page.evaluate(async () => { await navigator.serviceWorker.ready })
    await page.reload()
    await page.waitForFunction(() => navigator.serviceWorker.controller !== null)
    await waitForOpen(page, 1)

    // skipWaiting + clients.claim update the worker in this open document.
    serveOldWorker = false
    const activatedInPlace = await page.evaluate(async () => {
      const registration = await navigator.serviceWorker.getRegistration('/')
      const previous = navigator.serviceWorker.controller
      await new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error('service worker update timed out')), 5000)
        navigator.serviceWorker.addEventListener('controllerchange', () => {
          clearTimeout(timeout)
          resolve()
        }, { once: true })
        registration.update().catch(reject)
      })
      return navigator.serviceWorker.controller !== previous
    })
    expect(activatedInPlace).toBe(true)
    await context.unroute('**/sw.js')
    await context.setOffline(true)
    const fallback = await page.evaluate(async () => {
      const response = await fetch('/api/menu')
      return { status: response.status, body: await response.json() }
    })
    expect(fallback.status).toBe(503)
    expect(fallback.body.error.code).toBe('SERVER_UNAVAILABLE')
    await context.setOffline(false)

    const unavailableBefore = await statusCount(page, 503)
    const outage = await request.post('/__fixture/stream-outage?enabled=true')
    expect(outage.status()).toBe(200)
    expect((await outage.json()).disconnected).toBe(1)
    await expect.poll(() => page.evaluate(() => window.__agentDeckDashboard[0].source.readyState)).toBe(3)
    await expect.poll(() => statusCount(page, 503), { timeout: 10000 }).toBeGreaterThan(unavailableBefore)
    expect(await dashboardCount(page)).toBe(1)
    expect((await request.post('/__fixture/stream-outage?enabled=false')).status()).toBe(200)
    expect((await request.patch('/api/sessions/sess-003', { data: { title: RECOVERED_TITLE } })).status()).toBe(200)
    expect((await request.post('/__fixture/session/sess-002/status?to=waiting')).status()).toBe(204)
    await waitForOpen(page, 2)
    await expect(page.locator('.sess .tt', { hasText: RECOVERED_TITLE })).toHaveCount(1, { timeout: 10000 })
    await showCommandCenter(page)
    await expect(page.locator('[data-testid="cc-totals"]')).toContainText('0 running', { timeout: 10000 })
    await expect(page.locator('[data-testid="cc-totals"]')).toContainText('1 waiting')

    const beforeStopOutage = await statusCount(page, 503)
    const stopOutage = await request.post('/__fixture/stream-outage?enabled=true')
    expect((await stopOutage.json()).disconnected).toBe(1)
    await expect.poll(() => statusCount(page, 503), { timeout: 10000 }).toBeGreaterThan(beforeStopOutage)
    const stopped = await page.evaluate(async () => {
      const app = await import('/static/app/main.js')
      app.stopSSE()
      app.stopSSE()
      return { sources: window.__agentDeckDashboard.length, probes: window.__agentDeckMenuProbeStatuses.length }
    })
    expect((await request.post('/__fixture/stream-outage?enabled=false')).status()).toBe(200)
    await page.evaluate(() => {
      window.dispatchEvent(new Event('online'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await page.waitForTimeout(2500)
    expect(await dashboardCount(page)).toBe(stopped.sources)
    expect(await page.evaluate(() => window.__agentDeckMenuProbeStatuses.length)).toBe(stopped.probes)
    expect(await page.evaluate(() => window.__agentDeckDashboard.at(-1).source.readyState)).toBe(3)
  })

  test('an offline first handshake recovers through the app after browser wake', async ({ context, page, request }) => {
    await expandSeededCollapsedGroups(page)
    await page.goto('/')
    await page.evaluate(async () => { await navigator.serviceWorker.ready })
    await page.reload()
    await page.waitForFunction(() => navigator.serviceWorker.controller !== null)
    await waitForOpen(page, 1)
    await context.setOffline(true)
    await page.evaluate(async () => {
      const app = await import('/static/app/main.js')
      app.stopSSE()
      app.startSSE()
      app.startCommandCenterSSE()
    })
    await page.waitForFunction(() =>
      window.__agentDeckDashboard.length === 2 &&
      window.__agentDeckDashboard[1].errors > 0 &&
      window.__agentDeckDashboard[1].source.readyState === WebSocket.CLOSED,
    )
    expect((await request.patch('/api/sessions/sess-003', { data: { title: RECOVERED_TITLE } })).status()).toBe(200)
    await context.setOffline(false)
    await waitForOpen(page, 3)
    await expect(page.locator('.sess .tt', { hasText: RECOVERED_TITLE })).toHaveCount(1, { timeout: 10000 })
    // The other snapshot is received through that same recovered socket.
    await showCommandCenter(page)
    await expect(page.locator('[data-testid="cc-totals"]')).toBeVisible()
    expect(await dashboardCount(page)).toBe(3)
  })

  test('auth failures stop retries until a browser wake', async ({ page, request }) => {
    await page.goto('/')
    await waitForOpen(page, 1)
    const outage = await request.post('/__fixture/stream-outage?enabled=true&status=401')
    expect(outage.status()).toBe(200)
    expect((await outage.json()).disconnected).toBe(1)
    await expect.poll(() => statusCount(page, 401), { timeout: 10000 }).toBe(1)
    await page.waitForTimeout(2500)
    expect(await statusCount(page, 401)).toBe(1)
    expect(await dashboardCount(page)).toBe(1)
    expect((await request.post('/__fixture/stream-outage?enabled=false')).status()).toBe(200)
    await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')))
    await waitForOpen(page, 2)
  })
})
