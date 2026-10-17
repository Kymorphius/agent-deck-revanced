// matrix/hardened: transcript imports require an explicit click; controlled API,
// chart and theme mocks exercise recovery without network or canvas rendering.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createRequire } from 'node:module'
import { fireEvent, screen, waitFor } from '@testing-library/preact'
import { render as renderPreact } from 'preact'
import { html } from 'htm/preact'

// matrix/hardened: the configured component imports use CJS Preact, while RTL's
// renderer/act load another instance. Mount, flush and unmount with the same one.
const { act } = createRequire(import.meta.url)('preact/test-utils')

const mocked = vi.hoisted(() => ({ apiFetch: vi.fn() }))
vi.mock('../../../internal/web/static/app/api.js', () => ({ apiFetch: mocked.apiFetch }))
import { CostDashboard } from '../../../internal/web/static/app/CostDashboard.js'

const SUMMARY = '/api/costs/summary'
const DAILY = '/api/costs/daily?days=30'
const MODELS = '/api/costs/models'
const SYNC = '/api/costs/sync'
const makeSummary = () => ({
  today_usd: 12.34, today_events: 101,
  week_usd: 56.78, week_events: 102,
  month_usd: 90.12, month_events: 103,
  projected_usd: 123.45, auto_collect: false,
})
const makeDaily = () => [{ date: '2026-10-12', cost_usd: 12.34 }]
const makeModels = () => ({ 'original-model': 12.34 })
const makeSyncResult = () => ({ sessions_scanned: 7, events_imported: 11, events_skipped: 3, error_count: 2 })
const money = value => new Intl.NumberFormat(navigator.language, { style: 'currency', currency: 'USD' }).format(value)
const deferred = () => {
  let resolve
  const promise = new Promise(done => { resolve = done })
  return { promise, resolve }
}

let routes, chartInstances, observers, ChartMock, fetchMock, initialSummary, originalRootClass, mounts
const respond = (method, path, ...values) => routes.set(`${method} ${path}`, values)
const callsFor = (method, path) => mocked.apiFetch.mock.calls.filter(call => call[0] === method && call[1] === path)
const postCalls = () => mocked.apiFetch.mock.calls.filter(call => call[0] === 'POST')
const todayCard = () => screen.getByText('TODAY').closest('.stat')
const latestChart = type => chartInstances.filter(chart => chart.config.type === type).at(-1)

function render(vnode) {
  const container = document.createElement('div')
  document.body.appendChild(container)
  const view = {
    container,
    unmount() {
      if (!mounts.delete(view)) return
      act(() => renderPreact(null, container))
      container.remove()
    },
  }
  mounts.add(view)
  act(() => renderPreact(vnode, container))
  return view
}

// matrix/hardened: only observers attached to the root class are theme observers;
// Testing Library may also create observers while waiting for component updates.
class FakeMutationObserver {
  constructor(callback) {
    this.callback = callback
    this.active = false
    this.observe = vi.fn((target, options) => {
      this.target = target
      this.options = options
      this.active = true
    })
    this.disconnect = vi.fn(() => { this.active = false })
    observers.push(this)
  }
}

beforeEach(() => {
  mocked.apiFetch.mockReset()
  routes = new Map()
  chartInstances = []
  observers = []
  mounts = new Set()
  originalRootClass = document.documentElement.className
  initialSummary = makeSummary()
  respond('GET', SUMMARY, initialSummary)
  respond('GET', DAILY, makeDaily())
  respond('GET', MODELS, makeModels())
  respond('POST', SYNC, makeSyncResult())
  mocked.apiFetch.mockImplementation((method, path) => {
    const values = routes.get(`${method} ${path}`)
    if (!values?.length) return Promise.reject(new Error(`Unexpected API request: ${method} ${path}`))
    const value = values.length > 1 ? values.shift() : values[0]
    return value instanceof Error ? Promise.reject(value) : Promise.resolve(value)
  })
  ChartMock = vi.fn(function (canvas, config) {
    const chart = { canvas, config, destroy: vi.fn() }
    chartInstances.push(chart)
    return chart
  })
  fetchMock = vi.fn().mockRejectedValue(new Error('Unexpected real fetch'))
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('Chart', ChartMock)
  vi.stubGlobal('MutationObserver', FakeMutationObserver)
})

afterEach(() => {
  for (const view of [...mounts]) view.unmount()
  document.documentElement.className = originalRootClass
  vi.unstubAllGlobals()
})

async function mountReady() {
  const view = render(html`<${CostDashboard} />`)
  await waitFor(() => expect(todayCard()).toBeVisible())
  await waitFor(() => expect(ChartMock).toHaveBeenCalledTimes(2))
  return view
}

async function settle(...entries) {
  await act(async () => {
    for (const [gate, value] of entries) gate.resolve(value)
    await Promise.all(entries.map(([gate]) => gate.promise))
  })
}

async function changeTheme() {
  await act(async () => {
    document.documentElement.classList.toggle('dark')
    for (const observer of [...observers]) {
      if (observer.active && observer.target === document.documentElement
        && observer.options?.attributeFilter?.includes('class')) {
        observer.callback([{ type: 'attributes', attributeName: 'class', target: document.documentElement }], observer)
      }
    }
    await Promise.resolve()
  })
}

function expectSyncCounts(container) {
  expect(container).toHaveTextContent(/已扫描\D*7/)
  expect(container).toHaveTextContent(/导入\D*11/)
  expect(container).toHaveTextContent(/跳过\D*3/)
  expect(container).toHaveTextContent(/错误\D*2/)
}

describe('CostDashboard explicit manual synchronization', () => {
  it('only reads on mount and theme redraws, without posting an import', async () => {
    await mountReady()
    expect(callsFor('GET', SUMMARY)).toHaveLength(1)
    expect(callsFor('GET', DAILY)).toHaveLength(1)
    expect(callsFor('GET', MODELS)).toHaveLength(1)
    await changeTheme()
    await waitFor(() => expect(ChartMock).toHaveBeenCalledTimes(4))
    expect(callsFor('GET', SUMMARY)).toHaveLength(1)
    expect(callsFor('GET', DAILY)).toHaveLength(2)
    expect(callsFor('GET', MODELS)).toHaveLength(2)
    expect(postCalls()).toHaveLength(0)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it.each([
    ['false', false, '手动采集'],
    ['missing', undefined, '手动采集'],
    ['string true', 'true', '手动采集'],
    ['numeric true', 1, '手动采集'],
    ['boolean true', true, '自动采集已开启'],
  ])('shows the %s collection setting without automatically synchronizing', async (_name, autoCollect, label) => {
    if (autoCollect === undefined) delete initialSummary.auto_collect
    else initialSummary.auto_collect = autoCollect
    await mountReady()
    expect(screen.getByText(label)).toBeVisible()
    expect(screen.getByRole('button', { name: /同步费用/ })).toBeEnabled()
    expect(postCalls()).toHaveLength(0)
  })

  // matrix/hardened: two events in the same render turn require an in-flight
  // ref guard, rather than relying on the disabled state being painted first.
  it('coalesces rapid clicks, disables the pending action and reloads summary and charts after success', async () => {
    const pending = deferred()
    const refreshedSummary = { ...makeSummary(), today_usd: 98.76, today_events: 114 }
    respond('GET', SUMMARY, initialSummary, refreshedSummary)
    respond('GET', DAILY, makeDaily(), [{ date: '2026-10-13', cost_usd: 98.76 }])
    respond('GET', MODELS, makeModels(), { 'imported-model': 98.76 })
    respond('POST', SYNC, pending.promise)
    const view = await mountReady()
    const originalCharts = [...chartInstances]
    const button = screen.getByRole('button', { name: /同步费用/ })
    act(() => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(callsFor('POST', SYNC)).toHaveLength(1)
    expect(postCalls()).toHaveLength(1)
    expect(screen.getByRole('button', { name: /同步中/ })).toBeDisabled()
    expect(callsFor('GET', SUMMARY)).toHaveLength(1)
    expect(todayCard()).toHaveTextContent(money(initialSummary.today_usd))
    await settle([pending, makeSyncResult()])
    await waitFor(() => expect(todayCard()).toHaveTextContent(money(refreshedSummary.today_usd)))
    await waitFor(() => expect(ChartMock).toHaveBeenCalledTimes(4))
    expectSyncCounts(view.container)
    expect(screen.getByRole('button', { name: /同步费用/ })).toBeEnabled()
    expect(callsFor('GET', SUMMARY)).toHaveLength(2)
    expect(callsFor('GET', DAILY)).toHaveLength(2)
    expect(callsFor('GET', MODELS)).toHaveLength(2)
    expect(latestChart('line').config.data.datasets[0].data).toEqual([98.76])
    expect(latestChart('doughnut').config.data.labels).toEqual(['imported-model'])
    for (const chart of originalCharts) expect(chart.destroy).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it.each([
    ['HTTP 409', Object.assign(new Error('cost synchronization is already in progress'), { status: 409 })],
    ['rejected request', new Error('Network error: offline')],
  ])('shows a %s sync error while preserving the existing summary and charts', async (_name, error) => {
    respond('POST', SYNC, error)
    const view = await mountReady()
    const originalCharts = [...chartInstances]
    const canvases = [...view.container.querySelectorAll('canvas')]
    fireEvent.click(screen.getByRole('button', { name: /同步费用/ }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(error.message))
    expect(todayCard()).toHaveTextContent(money(initialSummary.today_usd))
    expect([...view.container.querySelectorAll('canvas')]).toEqual(canvases)
    expect(screen.getByRole('button', { name: /同步费用/ })).toBeEnabled()
    expect(screen.queryByText('Cost tracking unavailable')).not.toBeInTheDocument()
    expect(callsFor('POST', SYNC)).toHaveLength(1)
    expect(callsFor('GET', SUMMARY)).toHaveLength(1)
    expect(callsFor('GET', DAILY)).toHaveLength(1)
    expect(callsFor('GET', MODELS)).toHaveLength(1)
    expect(ChartMock).toHaveBeenCalledTimes(2)
    for (const chart of originalCharts) expect(chart.destroy).not.toHaveBeenCalled()
  })

  it('retains successful sync counts and old summary when summary refresh fails, while reloading charts', async () => {
    const refreshError = new Error('Summary refresh unavailable')
    respond('GET', SUMMARY, initialSummary, refreshError)
    const view = await mountReady()
    fireEvent.click(screen.getByRole('button', { name: /同步费用/ }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(refreshError.message))
    await waitFor(() => expect(callsFor('GET', DAILY)).toHaveLength(2))
    await waitFor(() => expect(callsFor('GET', MODELS)).toHaveLength(2))
    expectSyncCounts(view.container)
    expect(todayCard()).toHaveTextContent(money(initialSummary.today_usd))
    expect(screen.queryByText('Cost tracking unavailable')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /同步费用/ })).toBeEnabled()
    expect(callsFor('GET', SUMMARY)).toHaveLength(2)
    expect(callsFor('POST', SYNC)).toHaveLength(1)
  })

  // matrix/hardened: an abandoned sync cannot start a fresh summary/chart read,
  // even if the request succeeds after the component has been removed.
  it('does not issue follow-up GETs or create charts when a pending POST resolves after unmount', async () => {
    const pending = deferred()
    respond('POST', SYNC, pending.promise)
    const view = await mountReady()
    fireEvent.click(screen.getByRole('button', { name: /同步费用/ }))
    expect(callsFor('POST', SYNC)).toHaveLength(1)
    view.unmount()
    const callsBeforeResolution = mocked.apiFetch.mock.calls.length
    await settle([pending, makeSyncResult()])
    expect(mocked.apiFetch.mock.calls).toHaveLength(callsBeforeResolution)
    expect(ChartMock).toHaveBeenCalledTimes(2)
    for (const chart of chartInstances) expect(chart.destroy).toHaveBeenCalledTimes(1)
    expect(screen.queryByText('TODAY')).not.toBeInTheDocument()
  })

  it('ignores a pending post-sync summary refresh after unmount', async () => {
    const pending = deferred()
    respond('GET', SUMMARY, initialSummary, pending.promise)
    const view = await mountReady()
    fireEvent.click(screen.getByRole('button', { name: /同步费用/ }))
    await waitFor(() => expect(callsFor('GET', SUMMARY)).toHaveLength(2))
    await waitFor(() => expect(ChartMock).toHaveBeenCalledTimes(4))
    expect(todayCard()).toHaveTextContent(money(initialSummary.today_usd))
    expect(screen.getByRole('button', { name: /同步中/ })).toBeDisabled()
    view.unmount()
    const callsBeforeResolution = mocked.apiFetch.mock.calls.length
    await settle([pending, { ...makeSummary(), today_usd: 999 }])
    expect(mocked.apiFetch.mock.calls).toHaveLength(callsBeforeResolution)
    expect(ChartMock).toHaveBeenCalledTimes(4)
    expect(screen.queryByText('TODAY')).not.toBeInTheDocument()
  })

  it('does not load chart data when the initial summary resolves after unmount', async () => {
    const pending = deferred()
    respond('GET', SUMMARY, pending.promise)
    const view = render(html`<${CostDashboard} />`)
    expect(screen.getByText('Loading cost data…')).toBeVisible()
    expect(callsFor('GET', SUMMARY)).toHaveLength(1)
    view.unmount()
    await settle([pending, initialSummary])
    expect(mocked.apiFetch).toHaveBeenCalledTimes(1)
    expect(callsFor('GET', DAILY)).toHaveLength(0)
    expect(callsFor('GET', MODELS)).toHaveLength(0)
    expect(ChartMock).not.toHaveBeenCalled()
    expect(postCalls()).toHaveLength(0)
  })

  it('does not construct charts from pending chart GETs after unmount', async () => {
    const daily = deferred()
    const models = deferred()
    respond('GET', DAILY, daily.promise)
    respond('GET', MODELS, models.promise)
    const view = render(html`<${CostDashboard} />`)
    await waitFor(() => expect(callsFor('GET', DAILY)).toHaveLength(1))
    expect(callsFor('GET', MODELS)).toHaveLength(1)
    view.unmount()
    await settle([daily, makeDaily()], [models, makeModels()])
    expect(ChartMock).not.toHaveBeenCalled()
    expect(postCalls()).toHaveLength(0)
  })

  // matrix/hardened: theme redraws have their own generation; resolving an
  // older request last must not destroy or overwrite the current chart data.
  it('keeps the newer theme charts when overlapping older chart requests finish last', async () => {
    const oldDaily = deferred()
    const oldModels = deferred()
    const newDaily = deferred()
    const newModels = deferred()
    respond('GET', DAILY, oldDaily.promise, newDaily.promise)
    respond('GET', MODELS, oldModels.promise, newModels.promise)
    render(html`<${CostDashboard} />`)
    await waitFor(() => expect(callsFor('GET', DAILY)).toHaveLength(1))
    expect(ChartMock).not.toHaveBeenCalled()
    await changeTheme()
    expect(callsFor('GET', DAILY)).toHaveLength(2)
    expect(callsFor('GET', MODELS)).toHaveLength(2)
    await settle([newDaily, [{ date: '2026-10-13', cost_usd: 22 }]], [newModels, { 'new-theme-model': 22 }])
    await waitFor(() => expect(ChartMock).toHaveBeenCalledTimes(2))
    const currentCharts = [...chartInstances]
    expect(latestChart('line').config.data.datasets[0].data).toEqual([22])
    expect(latestChart('doughnut').config.data.labels).toEqual(['new-theme-model'])
    await settle([oldDaily, [{ date: '2026-10-12', cost_usd: 1 }]], [oldModels, { 'old-theme-model': 1 }])
    expect(ChartMock).toHaveBeenCalledTimes(2)
    for (const chart of currentCharts) expect(chart.destroy).not.toHaveBeenCalled()
    expect(latestChart('line').config.data.datasets[0].data).toEqual([22])
    expect(latestChart('doughnut').config.data.labels).toEqual(['new-theme-model'])
    expect(postCalls()).toHaveLength(0)
  })

  it('keeps the existing unavailable state when the initial summary GET fails', async () => {
    respond('GET', SUMMARY, new Error('Cost tracker disabled'))
    render(html`<${CostDashboard} />`)
    await waitFor(() => expect(screen.getByText('Cost tracking unavailable')).toBeVisible())
    expect(screen.queryByText('TODAY')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /同步费用/ })).not.toBeInTheDocument()
    expect(callsFor('GET', DAILY)).toHaveLength(0)
    expect(callsFor('GET', MODELS)).toHaveLength(0)
    expect(ChartMock).not.toHaveBeenCalled()
    expect(postCalls()).toHaveLength(0)
  })

  it('keeps charts optional and silent when a chart GET fails after a usable summary', async () => {
    respond('GET', DAILY, new Error('Chart history unavailable'))
    render(html`<${CostDashboard} />`)
    await waitFor(() => expect(todayCard()).toBeVisible())
    await waitFor(() => expect(callsFor('GET', MODELS)).toHaveLength(1))
    expect(todayCard()).toHaveTextContent(money(initialSummary.today_usd))
    expect(screen.getByRole('button', { name: /同步费用/ })).toBeEnabled()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText('Cost tracking unavailable')).not.toBeInTheDocument()
    expect(ChartMock).not.toHaveBeenCalled()
    expect(postCalls()).toHaveLength(0)
  })
})
