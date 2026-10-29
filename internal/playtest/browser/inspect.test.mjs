import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, basename } from 'node:path'
import { inspectCommand, playtest } from './playtest.mjs'

test('inspection requests become the Engine commands', () => {
  for (const [request, command] of [
    [{ op: 'view' }, 'engine.inspect.view list'],
    [{ op: 'view', mode: 'targets' }, 'engine.inspect.target list'],
    [{ op: 'view', mode: 'create', id: 'side', anchor: 'held.pane' }, 'engine.inspect.view create side anchor held.pane'],
    [{ op: 'view', mode: 'orbit', id: 'side', yaw: 30, pitch: -10 }, 'engine.inspect.view orbit side 30 -10'],
    [{ op: 'view', mode: 'camera', id: 'side', camera: { position: [1, 2, 3], yawDegrees: 90, pitchDegrees: -5 } }, 'engine.inspect.view camera side 1 2 3 90 -5'],
    [{ op: 'view', mode: 'frame', id: 'side', framing: 'side' }, 'engine.inspect.view frame side side camera'],
    [{ op: 'view', mode: 'frame', id: 'side', framing: 'front', handle: 12 }, 'engine.inspect.view frame side front 12'],
    [{ op: 'view', mode: 'frame', id: 'side', framing: 'underside', target: 'off-grip' }, 'engine.inspect.view frame side underside target off-grip'],
    [{ op: 'overlay', mode: 'show', kind: 'set', name: 'inspect.targets', view: 'player' }, 'engine.inspect.overlay show set inspect.targets in player'],
    [{ op: 'overlay', mode: 'hide' }, 'engine.inspect.overlay hide'],
    [{ op: 'overlay', mode: 'show', kind: 'bounds', handle: 4 }, 'engine.inspect.overlay show bounds 4'],
    [{ op: 'scene', depth: 0 }, 'engine.scene.objects * 0'],
    [{ op: 'scene', handle: 12, layer: 'viewmodel', page: 1 }, 'engine.scene.objects 12 --layer viewmodel --page 1'],
    [{ op: 'scene', mode: 'object', handle: 12 }, 'engine.scene.object 12'],
    [{ op: 'scene', mode: 'composition' }, 'engine.scene.composition'],
  ]) assert.equal(inspectCommand(request), command)
  for (const bad of [{ op: 'view', mode: 'create' }, { op: 'view', mode: 'frame', id: 'side', framing: 'sideways' }, { op: 'view', mode: 'create', id: 'a b' },
    { op: 'overlay', mode: 'show', kind: 'joints' }, { op: 'overlay', mode: 'show', kind: 'camera', view: 'side' }, { op: 'scene', mode: 'object' }, { op: 'scene', handle: -1 }])
    assert.throws(() => inspectCommand(bad))
})

function frameBytes(sequence, step) {
  const bytes = new Uint8Array(43)
  const view = new DataView(bytes.buffer)
  bytes.set([82, 83, 70, 49])
  view.setUint32(4, 40, true); view.setBigUint64(8, BigInt(sequence), true); view.setBigUint64(16, BigInt(step), true)
  view.setUint32(24, 4, true); view.setUint32(28, 2, true); view.setUint8(32, 3); view.setUint8(33, 1); view.setUint32(36, 3, true)
  bytes.set([7, 8, 9], 40)
  return bytes
}

// A product host behind the page: debug commands and frames/capture.
function fakeHost() {
  const commands = [], captures = [], views = new Set()
  const fetch = async (url, options = {}) => {
    const { pathname, searchParams, search } = new URL(url)
    if (pathname.endsWith('/debug/execute')) {
      const command = options.body
      commands.push(command)
      const [, verb, id] = command.split(' ')
      if (command.startsWith('engine.inspect.view ')) {
        if (verb === 'create') views.add(id)
        else if (verb !== 'list' && !views.has(id)) return new Response(`INSPECT_NO_VIEW: no inspection view ${id}`, { status: 422 })
        if (verb === 'close') views.delete(id)
        return Response.json({ views: views.size })
      }
      return Response.json({})
    }
    if (pathname.endsWith('/frames/capture')) {
      captures.push(search.slice(1))
      const view = searchParams.get('view')
      const headers = { 'X-Rusty-Frame-Cameras': '[{"position":[0,1,0]}]' }
      if (view) headers['X-Rusty-Frame-View'] = JSON.stringify({ id: view, presentationSeconds: 0.5 })
      if (searchParams.get('overlays') === '1') headers['X-Rusty-Frame-Overlays'] = '{"inspect.targets":3}'
      return new Response(frameBytes(captures.length, 6), { headers })
    }
    return new Response('not found', { status: 404 })
  }
  return { fetch, commands, captures }
}

test('a side view of the held assembly is captured with the player view at one held step and closed', async t => {
  const host = fakeHost()
  t.mock.method(globalThis, 'fetch', host.fetch)
  const page = { url: () => 'http://127.0.0.1:30300/play' }
  const directory = await mkdtemp(join(tmpdir(), 'inspect-'))
  await playtest(page, directory, { op: 'view', mode: 'create', id: 'side' })
  await playtest(page, directory, { op: 'view', mode: 'frame', id: 'side', framing: 'side' })
  await playtest(page, directory, { op: 'overlay', mode: 'show', kind: 'sets', view: 'side' })
  const result = await playtest(page, directory, { op: 'view', mode: 'capture', id: 'side', player: true, overlays: true })
  assert.equal(result.sameStep, true)
  assert.match(basename(result.view.path), /^view-side-step-6-/)
  assert.match(basename(result.player.path), /^player-step-6-/)
  assert.equal(result.view.headers['X-Rusty-Frame-Overlays'], '{"inspect.targets":3}')
  assert.equal(result.view.frame.view.id, 'side')
  assert.equal(result.player.headers['X-Rusty-Frame-View'], undefined)
  assert.match(await readFile(result.view.metadataPath, 'utf8'), /X-Rusty-Frame-View/)
  assert.deepEqual([...await readFile(result.view.path)], [7, 8, 9])
  assert.deepEqual(host.captures, ['format=png&view=side&overlays=1', 'format=png&overlays=1'])
  await playtest(page, directory, { op: 'view', mode: 'close', id: 'side' })
  assert.deepEqual(host.commands, ['engine.inspect.view create side', 'engine.inspect.view frame side side camera', 'engine.inspect.overlay show sets in side', 'engine.inspect.view close side'])
  await assert.rejects(playtest(page, directory, { op: 'view', mode: 'close', id: 'side' }), /refused: INSPECT_NO_VIEW: no inspection view side/)
})
