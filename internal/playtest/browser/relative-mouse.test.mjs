import test from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:http'

const executable = process.env.PLAYTEST_CHROMIUM

test('trusted pointer-lock deltas survive edges, unlock, and navigation', { skip: !executable, timeout: 30000 }, async () => {
  const server = createServer((req, res) => res.end(`<!doctype html><button id="lock" onclick="this.requestPointerLock()">Lock</button><output id="result">[]</output><script>
    const events = [];
    document.addEventListener('mousemove', e => {
      if (!document.pointerLockElement) return;
      events.push([e.movementX,e.movementY,e.isTrusted]);
      document.querySelector('#result').textContent=JSON.stringify(events);
    });
    document.addEventListener('keydown', e => { if(e.key==='u') document.exitPointerLock(); });
    window.__rustyPlaytest = async () => { await document.querySelector('#lock').requestPointerLock(); return {}; };
  </script>`))
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const dir = await mkdtemp(join(tmpdir(), 'relative-mouse-'))
  const worker = spawn(process.execPath, [new URL('./worker.mjs', import.meta.url).pathname], { stdio: ['pipe', 'pipe', 'inherit'] })
  const pending = new Map()
  let id = 0
  createInterface({ input: worker.stdout }).on('line', line => {
    const msg = JSON.parse(line)
    const receipt = pending.get(msg.id)
    pending.delete(msg.id)
    if (msg.error) receipt.reject(new Error(typeof msg.error === 'string' ? msg.error : JSON.stringify(msg.error)))
    else receipt.resolve(msg.result)
  })
  const rpc = (method, params = {}) => new Promise((resolve, reject) => {
    pending.set(++id, { resolve, reject })
    worker.stdin.write(JSON.stringify({ id, method, params }) + '\n')
  })
  const input = steps => rpc('input', { steps })
  const move = (dx, dy) => ({ kind: 'move', dx, dy })
  const lock = async () => {
    await rpc('browser', { op: 'click', selector: '#lock' })
    for (let i = 0; i < 50; i++) {
      if ((await rpc('status')).pointer_lock) return
      await new Promise(resolve => setTimeout(resolve, 10))
    }
    assert.fail('pointer lock was not acquired')
  }
  const events = async () => JSON.parse((await rpc('browser', { op: 'inspect', selector: '#result' })).targets[0].text)
  try {
    await rpc('launch', { width: 640, height: 480, user_data_dir: dir, executable_path: executable })
    const url = `http://127.0.0.1:${server.address().port}/`
    await rpc('navigate', { url })
    await assert.rejects(input([move(10, 5)]), /requires pointer lock/)
    await lock()
    // Batch validation must reject before delivering the first valid step.
    await assert.rejects(input([move(10, 5), move(0.5, 0)]), /integer/)
    assert.deepEqual(await events(), [])
    await input([move(10, 5), move(10000, -10000), move(-10000, 10000), move(-10, -5), move(0, 0)])
    assert.deepEqual(await events(), [[10, 5, true], [10000, -10000, true], [-10000, 10000, true], [-10, -5, true], [0, 0, true]])
    await rpc('browser', { op: 'press', key: 'u' })
    await assert.rejects(input([move(10, 5)]), /requires pointer lock/)
    await lock()
    await input([move(7, -3)])
    assert.deepEqual((await events()).at(-1), [7, -3, true])
    await rpc('navigate', { url })
    await lock()
    await input([move(-9, 11)])
    assert.deepEqual(await events(), [[-9, 11, true]])
    // A lock taken without a mouse event, as the Engine's assisted capture
    // takes it, still moves by exact deltas.
    await rpc('navigate', { url })
    await rpc('browser', { op: 'playtest', request: { op: 'lock' } })
    assert.equal((await rpc('status')).pointer_lock, true)
    await input([move(12, -4), move(-3, 8)])
    assert.deepEqual((await events()).filter(([x, y]) => x !== 0 || y !== 0), [[12, -4, true], [-3, 8, true]])
  } finally {
    await rpc('release')
    worker.stdin.end()
    await new Promise(resolve => worker.on('exit', resolve))
    await new Promise(resolve => server.close(resolve))
    await rm(dir, { recursive: true, force: true })
  }
})
