// matrix/hardened: finger-tracking + momentum touch scroll (terminalTouchScroll.js).
import { describe, it, expect } from 'vitest'
import { createTouchScroller } from '../../../internal/web/static/app/terminalTouchScroll.js'

const CELL = 20

// A fake requestAnimationFrame clock: frames run only when the test flushes.
function harness() {
  const steps = []
  let queue = []
  let nextId = 1
  const scroller = createTouchScroller({
    step: (n) => steps.push(n),
    cellHeight: () => CELL,
    raf: (fn) => { const id = nextId++; queue.push({ id, fn }); return id },
    cancelRaf: (id) => { queue = queue.filter(f => f.id !== id) },
  })
  const runFrames = (from, count, dt = 16) => {
    let t = from
    for (let i = 0; i < count && queue.length; i++) {
      t += dt
      const q = queue; queue = []
      q.forEach(f => f.fn(t))
    }
    return t
  }
  const lines = () => steps.reduce((a, b) => a + b, 0)
  return { scroller, steps, lines, runFrames, pending: () => queue.length }
}

describe('createTouchScroller tracking', () => {
  it('scrolls one line per cell height of finger travel, carrying the remainder', () => {
    const h = harness()
    h.scroller.start(500, 0)
    // 7px moves, slow (no fling): 10 moves = 70px = 3 lines, 10px carried
    for (let i = 1; i <= 10; i++) h.scroller.move(500 - 7 * i, i * 100)
    expect(h.lines()).toBe(3)
    h.scroller.move(500 - 80, 1100) // +10px completes the 4th line
    expect(h.lines()).toBe(4)
  })

  it('finger moving down scrolls back (negative lines)', () => {
    const h = harness()
    h.scroller.start(100, 0)
    h.scroller.move(160, 500)
    expect(h.lines()).toBe(-3)
  })

  it('a slow release does not fling', () => {
    const h = harness()
    h.scroller.start(500, 0)
    h.scroller.move(480, 200)
    h.scroller.move(460, 400)
    h.scroller.end(410)
    expect(h.pending()).toBe(0)
    expect(h.scroller.momentum).toBe(false)
  })

  it('a finger that paused before lifting does not fling', () => {
    const h = harness()
    h.scroller.start(500, 0)
    h.scroller.move(400, 16)
    h.scroller.move(300, 32)
    h.scroller.end(200) // lifted 168ms after the last move
    expect(h.scroller.momentum).toBe(false)
  })
})

describe('createTouchScroller momentum', () => {
  function flick(h) {
    h.scroller.start(600, 0)
    for (let i = 1; i <= 6; i++) h.scroller.move(600 - 30 * i, i * 16) // 1.875 px/ms upward
    h.scroller.end(100)
  }

  it('keeps scrolling in the flick direction after release, then stops on its own', () => {
    const h = harness()
    flick(h)
    const atRelease = h.lines()
    expect(h.scroller.momentum).toBe(true)
    h.runFrames(100, 600)
    expect(h.lines()).toBeGreaterThan(atRelease + 5)
    expect(h.steps.every(n => n > 0)).toBe(true)
    expect(h.scroller.momentum).toBe(false)
  })

  it('decelerates: the first 20 frames scroll further than the next 20', () => {
    const h = harness()
    flick(h)
    const a = h.lines()
    const t = h.runFrames(100, 20)
    const b = h.lines()
    h.runFrames(t, 20)
    const c = h.lines()
    expect(b - a).toBeGreaterThan(c - b)
    expect(c - b).toBeGreaterThan(0)
  })

  it('a new touch stops a running fling', () => {
    const h = harness()
    flick(h)
    h.runFrames(100, 3)
    h.scroller.start(300, 200)
    const n = h.lines()
    expect(h.pending()).toBe(0)
    h.runFrames(200, 50)
    expect(h.lines()).toBe(n)
  })

  it('caps the lines sent in one frame', () => {
    const h = harness()
    h.scroller.start(2000, 0)
    h.scroller.move(1000, 16) // 1000px in one frame = 50 lines
    expect(Math.max(...h.steps)).toBeLessThanOrEqual(8)
  })
})
