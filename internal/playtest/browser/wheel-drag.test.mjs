import test from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:http'

const executable = process.env.PLAYTEST_CHROMIUM

test('wheel steps scroll and down/point/up composes a right-button drag', { skip: !executable, timeout: 30000 }, async () => {
  const server = createServer((req, res) => res.end(`<!doctype html><body style="margin:0;height:100vh" oncontextmenu="return false"><output id="result">[]</output><script>
    const events = [];
    const log = e => { events.push(e); document.querySelector('#result').textContent = JSON.stringify(events); };
    document.addEventListener('wheel', e => log(['wheel', e.deltaX, e.deltaY, e.isTrusted]), { passive: true });
    document.addEventListener('mousedown', e => log(['down', e.button, e.clientX, e.clientY]));
    document.addEventListener('mousemove', e => { if (e.buttons) log(['drag', e.buttons, e.clientX, e.clientY]); });
    document.addEventListener('mouseup', e => log(['up', e.button, e.clientX, e.clientY]));
  </script></body>`))
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const dir = await mkdtemp(join(tmpdir(), 'wheel-drag-'))
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
  const point = (x, y) => ({ kind: 'point', x, y, width: 640, height: 480 })
  const events = async () => JSON.parse((await rpc('browser', { op: 'inspect', selector: '#result' })).targets[0].text)
  try {
    await rpc('launch', { width: 640, height: 480, user_data_dir: dir, executable_path: executable })
    await rpc('navigate', { url: `http://127.0.0.1:${server.address().port}/` })
    await assert.rejects(input([{ kind: 'wheel', dy: 0.5 }]), /integer/)
    await input([point(320, 240), { kind: 'wheel', dy: -120 }, { kind: 'wheel', dx: 30, dy: 240, ms: 20 }])
    await new Promise(resolve => setTimeout(resolve, 100))
    const wheels = (await events()).filter(e => e[0] === 'wheel')
    assert.deepEqual(wheels.map(e => e.slice(1)), [[0, -120, true], [30, 240, true]])
    await input([point(100, 100), { kind: 'down', button: 3 }, point(200, 120), point(300, 140), { kind: 'up', button: 3 }])
    // A button left down is lifted when its batch ends.
    await input([point(50, 50), { kind: 'down', button: 1 }, point(60, 50)])
    await new Promise(resolve => setTimeout(resolve, 100))
    const drag = (await events()).filter(e => e[0] !== 'wheel')
    assert.deepEqual(drag, [
      ['down', 2, 100, 100], ['drag', 2, 200, 120], ['drag', 2, 300, 140], ['up', 2, 300, 140],
      ['down', 0, 50, 50], ['drag', 1, 60, 50], ['up', 0, 60, 50],
    ])
  } finally {
    await rpc('release')
    worker.stdin.end()
    await new Promise(resolve => worker.on('exit', resolve))
    await new Promise(resolve => server.close(resolve))
    await rm(dir, { recursive: true, force: true })
  }
})
