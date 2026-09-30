import test from 'node:test'
import assert from 'node:assert/strict'
import { correlatedScreenshot, parseFrame } from './playtest.mjs'

function frameBytes({ sequence = 7, step = 42, format = 1, flags = 1, extra = 0, payload = [1, 2, 3] } = {}) {
  const headerLength = 40 + extra
  const bytes = new Uint8Array(headerLength + payload.length)
  const view = new DataView(bytes.buffer)
  bytes.set([82, 83, 70, 49]) // RSF1
  view.setUint32(4, headerLength, true)
  view.setBigUint64(8, BigInt(sequence), true)
  view.setBigUint64(16, BigInt(step), true)
  view.setUint32(24, 1280, true)
  view.setUint32(28, 720, true)
  view.setUint8(32, format)
  view.setUint8(33, flags)
  view.setUint32(36, payload.length, true)
  bytes.set(payload, headerLength)
  return bytes
}

test('parses RSF1 frames and skips unknown header extensions', () => {
  const frame = parseFrame(frameBytes({ extra: 8, flags: 3 }))
  assert.deepEqual({ ...frame, payload: [...frame.payload] },
    { sequence: 7, step: 42, width: 1280, height: 720, format: 'jpeg', held: true, video: true, payload: [1, 2, 3] })
  assert.equal(parseFrame(frameBytes({ format: 2, flags: 0 })).format, 'rgba8')
  assert.throws(() => parseFrame(new Uint8Array(40)), /not an RSF1 frame/)
  assert.throws(() => parseFrame(frameBytes().subarray(0, 41)), /truncated/)
})

function page(sequences) {
  let index = 0
  return {
    evaluate: async () => {
      const sequence = sequences[Math.min(index++, sequences.length - 1)]
      return sequence === null ? null : { sequence, step: sequence * 10, held: true, video: false, width: 1280, height: 720, cssWidth: 1280 }
    },
    screenshot: async () => {},
  }
}

test('a screenshot names its frame only when the shown frame did not change', async () => {
  assert.deepEqual(await correlatedScreenshot(page([5, 5]), '/x.png'),
    { path: '/x.png', frame: { sequence: 5, step: 50, held: true, video: false }, frameCorrelation: 'frame-sequence' })
  const moved = await correlatedScreenshot(page([5, 6]), '/x.png')
  assert.equal(moved.frame, null)
  assert.match(moved.frameCorrelation, /^uncertain/)
  assert.equal((await correlatedScreenshot(page([null]), '/x.png')).frameCorrelation, 'not an Engine stream page')
  assert.equal((await correlatedScreenshot(page([0, 0]), '/x.png')).frameCorrelation, 'no frame shown yet')
})
