import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { createInterface } from 'node:readline'
import test from 'node:test'

const worker = join(dirname(fileURLToPath(import.meta.url)), 'worker.mjs')

/** Start a worker and expose JSONL output records to a fake Go parent. */
function start(source) {
  const child = spawn(process.execPath, [worker], { stdio: ['pipe', 'pipe', 'pipe'] })
  const records = []
  const lines = createInterface({ input: child.stdout, crlfDelay: Infinity })
  let wake
  lines.on('line', line => {
    records.push(JSON.parse(line))
    wake?.()
    wake = undefined
  })
  child.stdin.write(`${JSON.stringify({ type: 'run', source })}\n`)
  return {
    child,
    records,
    reply(id, result, error) {
      const record = { id }
      if (result !== undefined) record.result = result
      if (error !== undefined) record.error = error
      child.stdin.write(`${JSON.stringify(record)}\n`)
    },
    async next(index) {
      while (records.length <= index) await new Promise(resolve => { wake = resolve })
      return records[index]
    },
    async terminal() {
      await once(child, 'exit')
      return records.at(-1)
    },
  }
}

test('serializes API calls and flushes asynchronous checkpoint and log calls', async () => {
  const run = start(`
    await controller.hold({ buttons: ['A'] }, 40)
    await keyboard.hold(['KeyW'], 20)
    await input([{ type: 'pointer', x: 4, y: 9 }])
    const image = await observe()
    checkpoint('looked', { image })
    await sleep(5)
    console.log('finished', image)
    return { image }
  `)
  const expected = [
    ['controller', { state: { buttons: ['A'] }, ms: 40 }, 'ok'],
    ['keyboard', { keys: ['KeyW'], ms: 20 }, 'ok'],
    ['input', { steps: [{ type: 'pointer', x: 4, y: 9 }] }, 'ok'],
    ['observe', {}, { frame: 7 }],
    ['checkpoint', { label: 'looked', data: { image: { frame: 7 } } }, 'ok'],
    ['sleep', { ms: 5 }, 'ok'],
    ['log', { args: ['finished', { frame: 7 }] }, 'ok'],
  ]
  for (let index = 0; index < expected.length; index++) {
    const actual = await run.next(index)
    const [method, args, result] = expected[index]
    assert.deepEqual(actual, { type: 'call', id: String(index + 1), method, args })
    if (index === 0) {
      await new Promise(resolve => setTimeout(resolve, 20))
      assert.equal(run.records.length, 1, 'a second input call raced ahead of the first response')
    }
    run.reply(actual.id, result)
  }
  assert.deepEqual(await run.terminal(), { type: 'done', result: { image: { frame: 7 } } })
})

test('reports awaited RPC failures as terminal errors', async () => {
  const run = start('await observe(); return 1')
  const call = await run.next(0)
  assert.equal(call.method, 'observe')
  run.reply(call.id, undefined, 'camera unavailable')
  const terminal = await run.terminal()
  assert.equal(terminal.type, 'done')
  assert.match(terminal.error, /camera unavailable/)
})

test('yield waits for the Go response before continuing', async () => {
  const run = start(`
    const answer = await yieldToAgent('confirm', { choice: 'continue' })
    return { answer }
  `)
  const yieldCall = await run.next(0)
  assert.deepEqual(yieldCall, {
    type: 'call', id: '1', method: 'yield', args: { label: 'confirm', data: { choice: 'continue' } },
  })
  await new Promise(resolve => setTimeout(resolve, 20))
  assert.equal(run.records.length, 1)
  run.reply(yieldCall.id, { resumed: true })
  assert.deepEqual(await run.terminal(), { type: 'done', result: { answer: { resumed: true } } })
})

test('interaction defaults to reticle options and forwards cursor options unchanged', async () => {
  const run = start(`
    const reticle = await interaction()
    const cursor = await interaction({ mode: 'cursor', x: 0.25, y: 0.75, aspect: 1.777 })
    return { reticle, cursor }
  `)
  const reticle = await run.next(0)
  assert.deepEqual(reticle, { type: 'call', id: '1', method: 'interaction', args: {} })
  run.reply(reticle.id, { candidate: 'crate' })
  const cursor = await run.next(1)
  assert.deepEqual(cursor, {
    type: 'call', id: '2', method: 'interaction', args: { mode: 'cursor', x: 0.25, y: 0.75, aspect: 1.777 },
  })
  run.reply(cursor.id, { candidate: 'door' })
  assert.deepEqual(await run.terminal(), {
    type: 'done', result: { reticle: { candidate: 'crate' }, cursor: { candidate: 'door' } },
  })
})

test('converts an unawaited API failure into a terminal error after flushing', async () => {
  const run = start(`
    observe()
    return 'source completed'
  `)
  const call = await run.next(0)
  assert.equal(call.method, 'observe')
  run.reply(call.id, undefined, 'capture failed')
  const terminal = await run.terminal()
  assert.equal(terminal.type, 'done')
  assert.match(terminal.error, /unawaited observe RPC failed:.*capture failed/)
})

test('returns source syntax and runtime errors as terminal records', async () => {
  for (const source of ['return (', 'throw new Error("script exploded")']) {
    const run = start(source)
    const terminal = await run.terminal()
    assert.equal(terminal.type, 'done')
    assert.match(terminal.error, /SyntaxError|script exploded/)
  }
})
