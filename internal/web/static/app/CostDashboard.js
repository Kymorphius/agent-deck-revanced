// CostDashboard.js -- In-app cost dashboard tab (Preact component)
// Replaces the standalone /costs page as an in-app tab with summary cards and Chart.js charts.
import { html } from 'htm/preact'
import { useEffect, useRef, useState } from 'preact/hooks'
import { apiFetch } from './api.js'

// Lazy-loader for Chart.js UMD bundle (issue #1022 part 2). Chart.js is
// ~206 KB and only the Costs route consumes it, so it must not ship in
// the initial payload. The first call injects a <script> tag pointing at
// the same /static/chart.umd.min.js asset the eager <script> used to
// load; subsequent calls return the cached promise so concurrent mounts
// don't race or re-fetch. Resolves with window.Chart once the global is
// set, or rejects if the script tag errors.
let chartLoaderPromise = null
function loadChartJs() {
  if (typeof window === 'undefined') return Promise.reject(new Error('no window'))
  if (window.Chart) return Promise.resolve(window.Chart)
  if (chartLoaderPromise) return chartLoaderPromise
  chartLoaderPromise = new Promise((resolve, reject) => {
    const s = document.createElement('script')
    s.src = '/static/chart.umd.min.js'
    s.async = true
    s.onload = () => {
      if (window.Chart) resolve(window.Chart)
      else reject(new Error('chart.umd.min.js loaded but window.Chart missing'))
    }
    s.onerror = () => {
      chartLoaderPromise = null
      reject(new Error('failed to load chart.umd.min.js'))
    }
    document.head.appendChild(s)
  })
  return chartLoaderPromise
}

// POL-5 (Phase 9, plan 02): locale-aware currency formatting. Constructed
// once at module load — Intl.NumberFormat is non-trivial to build (reads
// ICU data) and the user's locale does not change during a session. Both
// the summary card fmt() helper and the Chart.js y-axis tick callback
// delegate to this instance so they never drift. Currency stays USD
// regardless of locale (no conversion) — only symbol placement, digit
// grouping, and decimal separator follow navigator.language.
const currencyFormatter = new Intl.NumberFormat(navigator.language, {
  style: 'currency',
  currency: 'USD',
})

function fmt(v) {
  return currencyFormatter.format(v || 0)
}

// readChartTheme reads chart palette CSS variables from the document root.
// Variables are defined in internal/web/static/styles.src.css under :root
// (light) and html.dark (dark override). The MutationObserver wired up
// inside CostDashboard's useEffect re-runs buildCharts() whenever the
// theme class on <html> changes, so this helper produces the live palette.
// Per BUG #13 / UX-02 — replaces the legacy CHART_COLORS constant + isDark
// ternary that hardcoded every chart color in JS.
function readChartTheme() {
  const cs = getComputedStyle(document.documentElement)
  const v = (name, fallback) => (cs.getPropertyValue(name).trim() || fallback)
  return {
    text:        v('--chart-text',         '#6b7280'),
    grid:        v('--chart-grid',         '#e5e7eb'),
    legend:      v('--chart-legend',       '#374151'),
    primary:     v('--chart-primary',      '#2959aa'),
    primaryFill: v('--chart-primary-fill', 'rgba(41, 89, 170, 0.1)'),
    categorical: [
      v('--chart-categorical-1', '#7aa2f7'),
      v('--chart-categorical-2', '#bb9af7'),
      v('--chart-categorical-3', '#7dcfff'),
      v('--chart-categorical-4', '#9ece6a'),
      v('--chart-categorical-5', '#e0af68'),
      v('--chart-categorical-6', '#f7768e'),
      v('--chart-categorical-7', '#73daca'),
      v('--chart-categorical-8', '#ff9e64'),
    ],
  }
}

export function CostDashboard() {
  const [summary, setSummary] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)
  // matrix/hardened: viewing or repainting costs never imports transcripts.
  // Only the explicit button starts a mutation, fenced against late responses.
  const [syncing, setSyncing] = useState(false)
  const [syncResult, setSyncResult] = useState(null)
  const [syncError, setSyncError] = useState(null)
  const [chartRevision, setChartRevision] = useState(0)
  const mountedRef = useRef(false)
  const requestGenerationRef = useRef(0)
  const syncInFlightRef = useRef(false)

  const dailyCanvasRef = useRef(null)
  const modelCanvasRef = useRef(null)
  const dailyChartRef = useRef(null)
  const modelChartRef = useRef(null)

  // Load summary cards
  useEffect(() => {
    mountedRef.current = true
    const generation = ++requestGenerationRef.current
    const current = () => mountedRef.current && requestGenerationRef.current === generation
    apiFetch('GET', '/api/costs/summary')
      .then(data => {
        if (!current()) return
        setSummary(data)
        setLoading(false)
      })
      .catch(err => {
        if (!current()) return
        setError(err.message || 'Failed to load cost data')
        setLoading(false)
      })
    return () => {
      mountedRef.current = false
      requestGenerationRef.current += 1
    }
  }, [])

  // matrix/hardened: single-flight also uses a ref so two clicks before a render
  // cannot start two POSTs. Failed sync/refresh leaves the visible data intact.
  async function syncCosts() {
    if (!mountedRef.current || syncInFlightRef.current) return
    syncInFlightRef.current = true
    const generation = ++requestGenerationRef.current
    const current = () => mountedRef.current && requestGenerationRef.current === generation
    let synchronized = false
    setSyncing(true)
    setSyncResult(null)
    setSyncError(null)
    try {
      const result = await apiFetch('POST', '/api/costs/sync')
      if (!current()) return
      synchronized = true
      setSyncResult(result)
      // A summary reload failure must not suppress independent chart refreshes.
      setChartRevision(revision => revision + 1)
      const data = await apiFetch('GET', '/api/costs/summary')
      if (!current()) return
      setSummary(data)
    } catch (err) {
      if (!current()) return
      const detail = err.message || '请求失败'
      setSyncError(synchronized ? `费用已同步，但刷新失败：${detail}` : `同步费用失败：${detail}`)
    } finally {
      if (current()) {
        syncInFlightRef.current = false
        setSyncing(false)
      }
    }
  }

  // Build charts after loading (or when canvases become available)
  useEffect(() => {
    if (loading || error) return
    if (!dailyCanvasRef.current || !modelCanvasRef.current) return

    let cancelled = false
    // matrix/hardened: a late theme/data response must not replace newer charts.
    let buildGeneration = 0

    async function buildCharts() {
      const generation = ++buildGeneration
      try {
        const [Chart, dailyData, modelsData] = await Promise.all([
          loadChartJs(),
          apiFetch('GET', '/api/costs/daily?days=30'),
          apiFetch('GET', '/api/costs/models'),
        ])

        if (cancelled || generation !== buildGeneration) return

        // Destroy old chart instances before creating new ones
        if (dailyChartRef.current) {
          dailyChartRef.current.destroy()
          dailyChartRef.current = null
        }
        if (modelChartRef.current) {
          modelChartRef.current.destroy()
          modelChartRef.current = null
        }

        if (!dailyCanvasRef.current || !modelCanvasRef.current) return

        // Theme-aware chart colors read from CSS custom properties on
        // document.documentElement. The MutationObserver below re-runs
        // buildCharts() whenever the `dark` class toggles, so this read
        // always reflects the active theme without a page reload.
        const t = readChartTheme()

        const dates = dailyData || []
        const labels = dates.map(d => d.date.slice(5))
        const costs = dates.map(d => d.cost_usd)

        dailyChartRef.current = new Chart(dailyCanvasRef.current, {
          type: 'line',
          data: {
            labels,
            datasets: [{
              label: 'Daily Cost ($)',
              data: costs,
              borderColor: t.primary,
              backgroundColor: t.primaryFill,
              fill: true,
              tension: 0.3,
            }],
          },
          options: {
            responsive: true,
            plugins: { legend: { display: false } },
            scales: {
              x: { ticks: { color: t.text }, grid: { color: t.grid } },
              y: {
                ticks: { color: t.text, callback: v => currencyFormatter.format(v || 0) },
                grid: { color: t.grid },
              },
            },
          },
        })

        const models = modelsData || {}
        const mLabels = Object.keys(models)
        const mData = Object.values(models)

        modelChartRef.current = new Chart(modelCanvasRef.current, {
          type: 'doughnut',
          data: {
            labels: mLabels,
            datasets: [{
              data: mData,
              backgroundColor: t.categorical.slice(0, mLabels.length),
            }],
          },
          options: {
            responsive: true,
            plugins: {
              legend: {
                position: 'bottom',
                labels: { color: t.legend, font: { size: 11 } },
              },
            },
          },
        })
      } catch (_err) {
        // Charts are optional; summary cards still display
      }
    }

    buildCharts()

    // Re-build charts when the theme class on <html> changes (BUG #13 / UX-02).
    // The MutationObserver replaces the previous theme signal dep — the root
    // element class is the source of truth for theme, and reading via
    // getComputedStyle gives Chart.js the new CSS variable values without
    // requiring the parent to re-mount the component.
    const observer = new MutationObserver(() => {
      buildCharts()
    })
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })

    return () => {
      cancelled = true
      observer.disconnect()
    }
  }, [loading, error, chartRevision])

  // Cleanup chart instances on unmount
  useEffect(() => {
    return () => {
      if (dailyChartRef.current) {
        dailyChartRef.current.destroy()
        dailyChartRef.current = null
      }
      if (modelChartRef.current) {
        modelChartRef.current.destroy()
        modelChartRef.current = null
      }
    }
  }, [])

  if (loading) {
    return html`
      <div style="padding: 18px; font-family: var(--mono); font-size: 12px; color: var(--muted);">
        Loading cost data…
      </div>
    `
  }

  if (error) {
    return html`
      <div class="chart-card" style="margin: 14px;">
        <div class="title">Cost tracking unavailable</div>
        <div style="font-family: var(--mono); font-size: 12px; color: var(--text-dim); line-height: 1.6;">
          Start agent-deck with the cost tracker enabled to see spend, daily history, and per-model
          breakdowns here. The fixture binary intentionally runs without it.
        </div>
      </div>
    `
  }

  // matrix/hardened: show collection mode and an explicit action without
  // introducing polling or imports from the summary/chart effects.
  return html`
    <div style="display: flex; flex-direction: column; gap: 12px; flex: 1; min-height: 0; overflow: auto;">
      <div class="chart-card">
        <div style="display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap;">
          <div>
            <div class="title">${summary.auto_collect === true ? '自动采集已开启' : '手动采集'}</div>
            <div style="font-family: var(--mono); font-size: 12px; color: var(--muted);">点击同步可更新费用数据。</div>
          </div>
          <button type="button" class="btn primary" disabled=${syncing} onClick=${syncCosts}>
            ${syncing ? '同步中…' : '同步费用'}
          </button>
        </div>
        ${syncResult && html`
          <div role="status" aria-live="polite" style="margin-top: 8px; font-family: var(--mono); font-size: 12px; color: var(--muted);">
            同步完成：已扫描 ${syncResult.sessions_scanned ?? 0} 个会话；导入 ${syncResult.events_imported ?? 0}；跳过 ${syncResult.events_skipped ?? 0}；错误 ${syncResult.error_count ?? 0}
          </div>
        `}
        ${syncError && html`
          <div role="alert" style="margin-top: 8px; font-family: var(--mono); font-size: 12px; color: var(--danger, #f7768e);">${syncError}</div>
        `}
      </div>
      <div class="stat-grid">
        <div class="stat">
          <div class="lab">TODAY</div>
          <div class="val">${fmt(summary.today_usd)}</div>
          <div class="delta">${summary.today_events} events</div>
        </div>
        <div class="stat">
          <div class="lab">THIS WEEK</div>
          <div class="val">${fmt(summary.week_usd)}</div>
          <div class="delta">${summary.week_events} events</div>
        </div>
        <div class="stat">
          <div class="lab">THIS MONTH</div>
          <div class="val">${fmt(summary.month_usd)}</div>
          <div class="delta">${summary.month_events} events</div>
        </div>
        <div class="stat">
          <div class="lab">PROJECTED</div>
          <div class="val">${fmt(summary.projected_usd)}</div>
          <div class="delta">based on 7-day avg</div>
        </div>
      </div>
      <div style="display: grid; grid-template-columns: 2fr 1fr; gap: 12px;">
        <div class="chart-card">
          <div class="title">Daily spend · last 30 days</div>
          <canvas ref=${dailyCanvasRef}></canvas>
        </div>
        <div class="chart-card">
          <div class="title">Cost by model</div>
          <canvas ref=${modelCanvasRef}></canvas>
        </div>
      </div>
    </div>
  `
}
