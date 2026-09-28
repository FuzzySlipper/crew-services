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
