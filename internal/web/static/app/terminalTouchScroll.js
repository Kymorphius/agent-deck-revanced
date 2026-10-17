// terminalTouchScroll.js -- matrix/hardened: touch scrolling for the web
// terminal that tracks the finger and keeps a flick's momentum.
//
// The old handler turned every touchmove into a pixel WheelEvent. With mouse
// reporting on (tmux `mouse on`, and Claude Code's fullscreen UI on top of
// it), xterm converts pixels to lines and multiplies any event under 50px by
// 0.3 (CoreMouseService.consumeWheelEvent). A touchmove is a few pixels, so
// the finger travelled about three lines per wheel report, and Claude Code
// scrolls one line per report: the text crawled at a third of the finger's
// speed, and stopped dead on release.
//
// Here the finger's travel is counted in cell heights and each whole cell
// becomes one line-mode WheelEvent (deltaMode LINE, deltaY +-1), which xterm
// reports as exactly one wheel step. On release, the recent velocity carries
// on and decays, like a native scroll view. No DOM in the core so the
// behaviour is unit-testable with a fake clock.

const SAMPLE_WINDOW_MS = 100 // velocity is measured over the last 100ms of travel
const MIN_FLING_PX_PER_MS = 0.25 // slower releases just stop
const STOP_PX_PER_MS = 0.02
const FRICTION_PER_16MS = 0.94 // velocity kept per 60Hz frame
const MAX_LINES_PER_FRAME = 8 // keeps a hard flick from flooding the socket

// createTouchScroller tracks one finger. step(n) scrolls n lines (positive =
// toward newer output, i.e. the finger moving up); cellHeight() is the
// terminal's row height in CSS pixels; raf/cancelRaf schedule momentum frames.
export function createTouchScroller({ step, cellHeight, raf, cancelRaf }) {
  let lastY = 0
  let carry = 0 // travel not yet worth a whole line, in px
  let samples = [] // [{ y, t }] of the current gesture
  let velocity = 0 // px/ms, positive = finger moving up
  let frame = null
  let lastFrameT = 0

  function emit(px) {
    const cell = cellHeight()
    if (!(cell > 0)) return
    carry += px
    let lines = Math.trunc(carry / cell)
    if (lines === 0) return
    carry -= lines * cell
    if (lines > MAX_LINES_PER_FRAME) lines = MAX_LINES_PER_FRAME
    if (lines < -MAX_LINES_PER_FRAME) lines = -MAX_LINES_PER_FRAME
    step(lines)
  }

  function stopMomentum() {
    if (frame !== null) cancelRaf(frame)
    frame = null
    velocity = 0
  }

  function tick(t) {
    const dt = Math.min(t - lastFrameT, 64)
    lastFrameT = t
    emit(velocity * dt)
    velocity *= Math.pow(FRICTION_PER_16MS, dt / 16)
    if (Math.abs(velocity) < STOP_PX_PER_MS) {
      frame = null
      velocity = 0
      carry = 0
      return
    }
    frame = raf(tick)
  }

  return {
    start(y, t) {
      stopMomentum()
      lastY = y
      carry = 0
      samples = [{ y, t }]
    },
    move(y, t) {
      const dy = lastY - y
      lastY = y
      samples.push({ y, t })
      while (samples.length > 2 && t - samples[0].t > SAMPLE_WINDOW_MS) samples.shift()
      if (dy !== 0) emit(dy)
    },
    end(t) {
      const first = samples[0]
      const last = samples[samples.length - 1]
      samples = []
      // A finger that paused before lifting has no fling left in it.
      if (!first || !last || last.t === first.t || t - last.t > 60) return
      velocity = (first.y - last.y) / (last.t - first.t)
      if (Math.abs(velocity) < MIN_FLING_PX_PER_MS) { velocity = 0; return }
      lastFrameT = t
      frame = raf(tick)
    },
    cancel: stopMomentum,
    get momentum() { return frame !== null },
  }
}

// installTouchScroll wires a scroller to the terminal container. Each line
// is one line-mode WheelEvent on the xterm element, so xterm encodes it for
// whatever mouse protocol the pane asked for, or scrolls its own viewport when
// there is none. Listeners and any running fling end with controller.abort().
export function installTouchScroll(container, xtermEl, controller, cellHeight) {
  if (!container || !xtermEl) return
  const scroller = createTouchScroller({
    cellHeight,
    raf: (fn) => requestAnimationFrame(fn),
    cancelRaf: (id) => cancelAnimationFrame(id),
    step(lines) {
      const dir = lines > 0 ? 1 : -1
      for (let i = 0; i < Math.abs(lines); i++) {
        xtermEl.dispatchEvent(new WheelEvent('wheel', {
          deltaY: dir, deltaMode: WheelEvent.DOM_DELTA_LINE, bubbles: true, cancelable: true,
        }))
      }
    },
  })
  let active = false
  const opts = (passive) => ({ capture: true, passive, signal: controller.signal })

  container.addEventListener('touchstart', (e) => {
    if (!e.touches || e.touches.length !== 1) { active = false; scroller.cancel(); return }
    active = true
    scroller.start(e.touches[0].clientY, e.timeStamp)
  }, opts(true))
  container.addEventListener('touchmove', (e) => {
    if (!active || !e.touches || e.touches.length !== 1) return
    e.preventDefault()
    scroller.move(e.touches[0].clientY, e.timeStamp)
  }, opts(false))
  container.addEventListener('touchend', (e) => {
    if (!active) return
    active = false
    scroller.end(e.timeStamp)
  }, opts(true))
  container.addEventListener('touchcancel', () => { active = false; scroller.cancel() }, opts(true))
  controller.signal.addEventListener('abort', () => scroller.cancel())
}
