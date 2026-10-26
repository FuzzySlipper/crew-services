import test from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:http'

const executable = process.env.PLAYTEST_CHROMIUM

test('desktop resize preserves page state and captures actual viewport dimensions', { skip: !executable, timeout: 30000 }, async () => {
  const server = createServer((req, res) => res.end(`<!doctype html><body style="margin:0"><button id="count">0</button><script>
    document.querySelector('#count').onclick = e => e.target.textContent = Number(e.target.textContent) + 1;
  </script></body>`))
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const dir = await mkdtemp(join(tmpdir(), 'viewport-'))
  const worker = spawn(process.execPath, [new URL('./worker.mjs', import.meta.url).pathname], { stdio: ['pipe', 'pipe', 'inherit'] })
  const pending = new Map()
  let id = 0
  createInterface({ input: worker.stdout }).on('line', line => {
    const msg = JSON.parse(line), receipt = pending.get(msg.id)
    pending.delete(msg.id)
    if (msg.error) receipt.reject(new Error(typeof msg.error === 'string' ? msg.error : JSON.stringify(msg.error)))
    else receipt.resolve(msg.result)
  })
  const rpc = (method, params = {}) => new Promise((resolve, reject) => {
    pending.set(++id, { resolve, reject })
    worker.stdin.write(JSON.stringify({ id, method, params }) + '\n')
  })
  try {
    await rpc('launch', { width: 1280, height: 720, user_data_dir: dir, artifact_directory: dir, executable_path: executable })
    await rpc('navigate', { url: `http://127.0.0.1:${server.address().port}/` })
    await rpc('browser', { op: 'click', selector: '#count' })
    for (const [width, height] of [[1920, 1080], [2560, 1440], [1920, 1080]]) {
      const resized = await rpc('browser', { op: 'resize', width, height })
      assert.deepEqual(resized.viewport, { width, height, device_pixel_ratio: 1 })
      const inspected = await rpc('browser', { op: 'inspect', selector: '#count' })
      assert.equal(inspected.targets[0].text, '1', 'resize must not reload the page')
      const capture = await rpc('observe')
      const png = Buffer.from(capture.screenshot_base64, 'base64')
      assert.equal(png.readUInt32BE(16), width)
      assert.equal(png.readUInt32BE(20), height)
    }
    await assert.rejects(rpc('browser', { op: 'resize', width: 0, height: 1080 }), /viewport/)
    assert.equal((await rpc('status')).viewport.width, 1920, 'invalid request leaves viewport alone')
    await rpc('browser', { op: 'click', selector: '#count' })
    assert.equal((await rpc('browser', { op: 'inspect', selector: '#count' })).targets[0].text, '2', 'input still reaches the same page')
  } finally {
    await rpc('release'); worker.stdin.end()
    await new Promise(resolve => worker.on('exit', resolve))
    await new Promise(resolve => server.close(resolve))
    await rm(dir, { recursive: true, force: true })
  }
})
