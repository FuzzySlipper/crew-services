import test from 'node:test'
import assert from 'node:assert/strict'
import { playtest } from './playtest.mjs'
function fixture({ hold = false, fail = false } = {}) {
  const events = []
  let duration = 300
  const page = {
    keyboard: { down: async key => events.push(['down', key]), up: async key => events.push(['up', key]) },
    evaluate: async (_, request) => {
      events.push([request.op, request.ms])
      if (request.op === 'action') return { available: true, key: 'KeyE', durationMs: duration, hold }
      if (request.op === 'time') return { mode: 'action-driven', fixedStepHz: 50 }
      if (request.op === 'advance') { if (fail) throw new Error('uncertain transport'); return { advancedMs: request.ms } }
      return {}
    },
  }
  return { page, events, setDuration: value => { duration = value } }
}
test('tap advances once pressed, releases before recovery, resolves duration every invocation', async () => {
  const f = fixture()
  const first = await playtest(f.page, '/unused', { op: 'act', id: 'attack' })
  assert.equal(first.advancedMs, 300)
  assert.ok(f.events.findIndex(e => e[0] === 'up') < f.events.findIndex(e => e[0] === 'advance' && e[1] === 280))
  f.setDuration(600)
  assert.equal((await playtest(f.page, '/unused', { op: 'act', id: 'attack' })).advancedMs, 600)
})
test('held movement releases after all advancement', async () => {
  const f = fixture({ hold: true })
  await playtest(f.page, '/unused', { op: 'act', id: 'forward', ms: 200 })
  assert.ok(f.events.findIndex(e => e[0] === 'up') > f.events.findIndex(e => e[0] === 'advance' && e[1] === 180))
})
test('uncertain advance releases input and is never replayed', async () => {
  const f = fixture({ fail: true })
  const result = await playtest(f.page, '/unused', { op: 'act', id: 'attack' })
  assert.match(result.error, /uncertain/)
  assert.equal(result.accepted, false)
  assert.equal(result.inputReleased, true)
  assert.equal(f.events.filter(e => e[0] === 'advance').length, 1)
  assert.equal(f.events.filter(e => e[0] === 'up').length, 1)
})

test('jump pulses jump key, holds movement, releases before settling and reports observed endpoint', async () => {
  const f = fixture()
  const evaluate = f.page.evaluate
  f.page.evaluate = async (fn, r) => {
    if (r.op === 'jump-plan') return { available: true, yawDeltaDegrees: 90, settleMs: 200, action: { available: true, id: 'jump-toward', key: 'Space', heldKeys: ['KeyW'], hold: false, durationMs: 300 } }
    if (r.op === 'observe') return { player: { position: { x: 2, y: 1, z: 0 }, movement: { grounded: true } }, axes: { floorY: 0 } }
    return evaluate(fn, r)
  }
  const r = await playtest(f.page, '/unused', { op: 'jump', x: 2, y: 0, z: 0 })
  assert.equal(r.distanceToTargetFeet, 0)
  assert.equal(r.grounded, true)
  assert.equal(r.advancedMs, 500)
  assert.ok(f.events.findIndex(e => e[0] === 'up' && e[1] === 'Space') < f.events.findIndex(e => e[0] === 'up' && e[1] === 'w'))
  assert.ok(f.events.findIndex(e => e[0] === 'up' && e[1] === 'w') < f.events.findLastIndex(e => e[0] === 'advance'))
})

test('composed action attempts every release even when one key release fails', async () => {
  const f = fixture({ fail: true })
  const evaluate = f.page.evaluate
  f.page.evaluate = async (fn, request) => request.op === 'action'
    ? { available: true, key: 'Space', heldKeys: ['KeyW'], durationMs: 200, hold: false }
    : evaluate(fn, request)
  f.page.keyboard.up = async key => {
    f.events.push(['up', key])
    if (key === 'w') throw new Error('release transport failure')
  }
  const result = await playtest(f.page, '/unused', { op: 'act', id: 'jump-toward' })
  assert.equal(result.accepted, false)
  assert.equal(result.inputReleased, false)
  assert.match(result.releaseError, /release transport failure/)
  assert.deepEqual(f.events.filter(e => e[0] === 'up').map(e => e[1]), ['w', 'Space'])
  assert.equal(f.events.filter(e => e[0] === 'advance').length, 1)
})

test('pointer action uses current binding, releases before recovery, and never clicks to focus', async () => {
  const f = fixture()
  f.page.locator = () => ({ first: () => ({ focus: async () => {}, boundingBox: async () => ({ x: 0, y: 0, width: 100, height: 80 }) }) })
  f.page.mouse = {
    move: async (x, y) => f.events.push(['move', x, y]),
    down: async options => f.events.push(['pointer-down', options.button]),
    up: async options => f.events.push(['pointer-up', options.button]),
  }
  const evaluate = f.page.evaluate
  let key = 'Primary'
  f.page.evaluate = async (fn, request) => request.op === 'action'
    ? { available: true, key, durationMs: 300, hold: false }
    : evaluate(fn, request)
  const mouse = await playtest(f.page, '/unused', { op: 'act', id: 'attack' })
  assert.equal(mouse.inputPath, 'physical-pointer')
  assert.equal(mouse.inputReleased, true)
  assert.ok(f.events.findIndex(e => e[0] === 'pointer-up') < f.events.findIndex(e => e[0] === 'advance' && e[1] === 280))
  assert.equal(f.events.filter(e => e[0] === 'pointer-down').length, 1)
  key = 'KeyQ'
  const keyboard = await playtest(f.page, '/unused', { op: 'act', id: 'attack' })
  assert.equal(keyboard.inputPath, 'physical-keyboard')
  assert.ok(f.events.some(e => e[0] === 'down' && e[1] === 'q'))
})

test('uncertain pointer action attempts release without replay', async () => {
  const f = fixture({ fail: true })
  f.page.locator = () => ({ first: () => ({ focus: async () => {}, boundingBox: async () => ({ x: 0, y: 0, width: 100, height: 80 }) }) })
  f.page.mouse = { move: async () => {}, down: async () => f.events.push(['pointer-down']), up: async () => f.events.push(['pointer-up']) }
  const evaluate = f.page.evaluate
  f.page.evaluate = async (fn, request) => request.op === 'action'
    ? { available: true, key: 'Primary', durationMs: 300, hold: false }
    : evaluate(fn, request)
  const result = await playtest(f.page, '/unused', { op: 'act', id: 'attack' })
  assert.equal(result.accepted, false)
  assert.equal(result.inputReleased, true)
  assert.equal(f.events.filter(e => e[0] === 'pointer-down').length, 1)
  assert.equal(f.events.filter(e => e[0] === 'pointer-up').length, 1)
})
