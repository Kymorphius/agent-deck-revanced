// main.js -- Preact app entry point and full boot sequence
// Handles: auth token extraction, dashboard connection, route sync, service worker registration
import { render, html } from 'htm/preact'
import { App, applyPath } from './App.js'
import { apiFetch, authHeaders } from './api.js'
import {
  sessionsSignal,
  sessionsLoadedSignal,
  connectionSignal,
  authTokenSignal,
  commandCenterSignal,
  selectedGroupSignal,
  loadArchivedSessions,
} from './state.js'
import { addToast } from './Toast.js'
import { initPush } from './push.js'
// matrix/hardened: keep menu wire validation separate from transport lifecycle.
import { readMenuBaseline, applyMenuDelta } from './menuDelta.js'

// ---------- Auth token extraction ----------

;(function extractAuthToken() {
  const params = new URLSearchParams(window.location.search)
  const token = params.get('token')
  if (!token) return

  authTokenSignal.value = token

  // Strip token from URL so it isn't logged by the server or leaked via Referer header
  params.delete('token')
  const cleanSearch = params.toString()
  const cleanPath = window.location.pathname + (cleanSearch ? '?' + cleanSearch : '') + window.location.hash
  history.replaceState(null, '', cleanPath)

  // Prevent token from appearing in Referer headers on any subsequent navigation
  let meta = document.querySelector('meta[name="referrer"]')
  if (!meta) {
    meta = document.createElement('meta')
    meta.name = 'referrer'
    document.head.appendChild(meta)
  }
  meta.content = 'no-referrer'
})()

// ---------- Dashboard connection ----------
// matrix/hardened: one push-only WebSocket carries menu and Command Center.
// Short authenticated JSON probes handle recovery without occupying HTTP/1.1
// request slots with a second pair of long-lived streams. The exported SSE
// names remain compatible with existing callers.

let _dashboardSource = null
let _dashboardEnabled = false
let _dashboardNeedsRecovery = false
let _dashboardTerminalFailure = false

const DASHBOARD_RECOVERY_DELAYS_MS = [1000, 2000, 4000, 8000, 16000, 30000]
const DASHBOARD_PROBE_TIMEOUT_MS = 5000
let _dashboardRecoveryTimer = null
let _dashboardRecoveryAbort = null
let _dashboardRecoveryGeneration = 0
let _dashboardRecoveryAttempt = 0

function recoverableDashboard(includeTerminal = false) {
  return _dashboardEnabled && _dashboardSource && _dashboardNeedsRecovery &&
    (includeTerminal || !_dashboardTerminalFailure)
    ? _dashboardSource : null
}

async function probeDashboard(signal) {
  let response
  try {
    response = await fetch('/api/menu', {
      method: 'GET',
      headers: authHeaders(),
      cache: 'no-store',
      signal,
    })
    const contentType = (response.headers.get('Content-Type') || '').split(';', 1)[0].trim().toLowerCase()
    if (response.status === 200 && contentType === 'application/json') {
      let data
      try { data = await response.json() } catch (error) {
        return error.name === 'SyntaxError' ? 'terminal' : 'transient'
      }
      return data && !data.error && Array.isArray(data.items) ? 'ready' : 'terminal'
    }
    if (response.status === 408 || response.status === 425 || response.status === 429 || response.status >= 500) {
      return 'transient'
    }
    return 'terminal'
  } catch (_) {
    return 'transient'
  } finally {
    try { await response?.body?.cancel() } catch (_) { /* already consumed or aborted */ }
  }
}

function scheduleDashboardRecovery(delay) {
  if (_dashboardRecoveryTimer !== null || _dashboardRecoveryAbort || !recoverableDashboard()) return

  const wait = delay ?? DASHBOARD_RECOVERY_DELAYS_MS[Math.min(_dashboardRecoveryAttempt, DASHBOARD_RECOVERY_DELAYS_MS.length - 1)]
  const generation = _dashboardRecoveryGeneration
  _dashboardRecoveryTimer = setTimeout(async () => {
    _dashboardRecoveryTimer = null
    const source = recoverableDashboard()
    if (!source || generation !== _dashboardRecoveryGeneration) return

    const controller = new AbortController()
    _dashboardRecoveryAbort = controller
    const probeTimeout = setTimeout(() => controller.abort(), DASHBOARD_PROBE_TIMEOUT_MS)
    const outcome = await probeDashboard(controller.signal)
    clearTimeout(probeTimeout)
    if (_dashboardRecoveryAbort === controller) _dashboardRecoveryAbort = null
    if (generation !== _dashboardRecoveryGeneration || _dashboardSource !== source || !_dashboardEnabled) return

    _dashboardRecoveryAttempt = Math.min(_dashboardRecoveryAttempt + 1, DASHBOARD_RECOVERY_DELAYS_MS.length - 1)
    if (outcome === 'ready') {
      // Detach first: close/error callbacks from the retired socket cannot
      // disconnect or schedule another retry for its replacement.
      _dashboardSource = null
      source.close()
      startSSE()
    } else if (outcome === 'terminal') {
      _dashboardTerminalFailure = true
    } else {
      scheduleDashboardRecovery()
    }
  }, wait)
}

function markDashboardHealthy() {
  _dashboardRecoveryAttempt = 0
  connectionSignal.value = 'connected'
}

function wakeDashboardRecovery() {
  if (!recoverableDashboard(true)) return
  _dashboardRecoveryGeneration += 1
  if (_dashboardRecoveryTimer !== null) clearTimeout(_dashboardRecoveryTimer)
  _dashboardRecoveryTimer = null
  _dashboardRecoveryAbort?.abort()
  _dashboardRecoveryAbort = null
  _dashboardRecoveryAttempt = 0
  _dashboardTerminalFailure = false
  scheduleDashboardRecovery(0)
}

// Preserve completion de-duplication across reconnects.
const _ccSeenCompletions = new Set()

function applyMenuSnapshot(snapshot) {
  if (!snapshot || !Array.isArray(snapshot.items)) return false
  sessionsSignal.value = snapshot.items
  sessionsLoadedSignal.value = true
  // Group panels also show archived sessions from their separate endpoint.
  // A change-driven active snapshot refreshes that half while a panel is open.
  if (selectedGroupSignal.value) loadArchivedSessions()
  return true
}

function applyCommandCenterSnapshot(snapshot) {
  if (!snapshot || typeof snapshot !== 'object' || Array.isArray(snapshot)) return false
  commandCenterSignal.value = snapshot
  const done = Array.isArray(snapshot.recentlyCompleted) ? snapshot.recentlyCompleted : []
  for (const c of done) {
    const key = (c && (c.id || '')) + ':' + (c && (c.at || ''))
    if (_ccSeenCompletions.has(key)) continue
    _ccSeenCompletions.add(key)
    if (c && c.title) addToast(`✅ ${c.title} just finished`, 'success')
  }
  if (_ccSeenCompletions.size > 200) _ccSeenCompletions.clear()
  return true
}

export function startSSE() {
  _dashboardEnabled = true
  if (_dashboardSource) return

  const url = new URL('/ws/dashboard', window.location.href)
  url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  // matrix/hardened: older servers ignore this opt-in and keep sending full menus.
  url.searchParams.set('menu_protocol', 'delta-v1')
  const token = authTokenSignal.value
  if (token) url.searchParams.set('token', token)

  const source = new WebSocket(url.href)
  _dashboardSource = source
  _dashboardNeedsRecovery = false
  _dashboardTerminalFailure = false
  // matrix/hardened: only this socket's versioned full frame can establish its
  // delta baseline. Initial REST data and retired sockets never seed a chain.
  let menuBaseline = null
  const current = () => _dashboardEnabled && _dashboardSource === source
  const failed = () => {
    if (!current()) return
    menuBaseline = null
    _dashboardNeedsRecovery = true
    connectionSignal.value = 'disconnected'
    scheduleDashboardRecovery()
  }

  source.addEventListener('open', () => {
    if (current() && !_dashboardNeedsRecovery) markDashboardHealthy()
  })
  source.addEventListener('message', (event) => {
    if (!current() || _dashboardNeedsRecovery) return
    try {
      const message = JSON.parse(event.data)
      if (message.type === 'menu') {
        // matrix/hardened: full snapshots keep their existing display contract.
        // Invalid identities/indexes/totals downgrade to a visible full without
        // a delta baseline, matching the server's conservative full fallback.
        // Legacy frames without a version likewise cannot authorize deltas.
        menuBaseline = readMenuBaseline(message)
        if (applyMenuSnapshot(message.data)) markDashboardHealthy()
      } else if (message.type === 'menu-delta') {
        const next = applyMenuDelta(menuBaseline, message)
        if (!next) {
          // Keep the last good UI atomically, then use the existing authenticated
          // probe/reconnect flow to obtain a new full frame. No client WS input.
          failed()
          source.close()
          return
        }
        menuBaseline = next
        if (applyMenuSnapshot(next.snapshot)) markDashboardHealthy()
      } else if (message.type === 'command-center') {
        if (applyCommandCenterSnapshot(message.data)) markDashboardHealthy()
      } else if (message.type === 'resync') {
        // Browsers do not reconnect WebSockets. Explicitly schedule recovery
        // even when close is asynchronous or no network error is emitted.
        failed()
        source.close()
      } else if (message.type === 'error') {
        // matrix/hardened: the backend also resets its baseline after source
        // failure, so recovery must start with a full frame on this socket.
        menuBaseline = null
        connectionSignal.value = 'disconnected'
      }
    } catch (_) {
      // Ignore malformed or unknown frames; keep the last valid snapshots.
    }
  })
  source.addEventListener('error', () => {
    // A failed CONNECTING socket also emits close. Avoid closing it again
    // here; the single retry timer handles either event without duplication.
    failed()
  })
  source.addEventListener('close', failed)
}

export function stopSSE() {
  _dashboardEnabled = false
  _dashboardRecoveryGeneration += 1
  if (_dashboardRecoveryTimer !== null) clearTimeout(_dashboardRecoveryTimer)
  _dashboardRecoveryTimer = null
  _dashboardRecoveryAbort?.abort()
  _dashboardRecoveryAbort = null
  _dashboardRecoveryAttempt = 0
  _dashboardTerminalFailure = false
  _dashboardNeedsRecovery = false
  const source = _dashboardSource
  _dashboardSource = null
  source?.close()
  connectionSignal.value = 'disconnected'
}

export function startCommandCenterSSE() {
  startSSE()
}

window.addEventListener('online', wakeDashboardRecovery)
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') wakeDashboardRecovery()
})

// ---------- Initial menu load + dashboard kick-off ----------

export async function loadMenu() {
  const generation = _dashboardRecoveryGeneration
  try {
    const data = await apiFetch('GET', '/api/menu')
    if (generation !== _dashboardRecoveryGeneration) return
    sessionsSignal.value = data.items || []
    // POL-1: first real data arrived — unmount the skeleton. Do NOT set
    // this in the catch branch; the skeleton is the correct state when
    // we're offline.
    sessionsLoadedSignal.value = true
    startSSE()
    startCommandCenterSSE()
  } catch (_) {
    if (generation !== _dashboardRecoveryGeneration) return
    connectionSignal.value = 'disconnected'
    // Start the dashboard so its recovery can reconnect when the server returns
    startSSE()
    startCommandCenterSSE()
  }
}

// ---------- Route sync: URL -> selection ----------

export function applyRouteSelection() {
  const path = window.location.pathname || '/'
  // Don't force-clear at boot when the path is neither /s/ nor /g/.
  if (path === '/') return
  applyPath(path)
}

// ---------- Service worker registration ----------

export function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return

  function doRegister() {
    navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {
      // SW registration failure is non-fatal; app works without it
    })
  }

  if (document.readyState === 'complete' || document.readyState === 'interactive') {
    doRegister()
  } else {
    window.addEventListener('load', doRegister, { once: true })
  }
}

// ---------- Boot sequence ----------

const root = document.getElementById('app-root')
if (root) {
  root.style.cssText = 'position:fixed;inset:0;z-index:10;'
  applyRouteSelection()
  loadMenu()
  registerServiceWorker()
  initPush()
  render(html`<${App} />`, root)
}
