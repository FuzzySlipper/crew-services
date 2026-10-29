import { mkdir, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
const run = promisify(execFile)
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))

export async function engineCall(page, request) {
  return page.evaluate(async request => {
    if (typeof window.__rustyPlaytest !== 'function') throw new Error('capability_unavailable: Engine playtest inspection')
    return window.__rustyPlaytest(request)
  }, request)
}

// The Engine's streamed canvas names the runtime frame it shows. Reading it on
// both sides of a screenshot ties the composite image to a simulation step when
// the frame did not change in between (always the case while time is held).
export async function frameFacts(page) {
  return page.evaluate(() => {
    const canvas = document.querySelector('canvas[data-rusty-application-renderer="engine-owned"]')
    if (!canvas) return null
    const d = canvas.dataset
    return { sequence: Number(d.rustyFrameSequence ?? 0), step: Number(d.rustyFrameStep ?? 0), held: d.rustyFrameHeld === 'true', video: d.rustyFrameVideo === 'true',
      width: canvas.width, height: canvas.height, cssWidth: Math.max(1, Math.round(canvas.clientWidth)) }
  })
}

export async function correlatedScreenshot(page, path) {
  const before = await frameFacts(page).catch(() => null)
  await page.screenshot({ path, type: 'png' })
  const after = before ? await frameFacts(page).catch(() => null) : null
  if (!before || !after || before.sequence === 0) return { path, frame: null, frameCorrelation: before ? 'no frame shown yet' : 'not an Engine stream page' }
  if (before.sequence !== after.sequence) return { path, frame: null, frameCorrelation: `uncertain: frame ${before.sequence} became ${after.sequence} during capture` }
  const { sequence, step, held, video } = after
  return { path, frame: { sequence, step, held, video }, frameCorrelation: 'frame-sequence' }
}

// RSF1 header: magic, header length, sequence, step, width, height, format
// (1 JPEG, 2 RGBA8), flags (1 held, 2 video). Later fields extend the header.
export function parseFrame(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  if (bytes.byteLength < 40 || String.fromCharCode(...bytes.subarray(0, 4)) !== 'RSF1') throw new Error('not an RSF1 frame')
  const headerLength = view.getUint32(4, true), payloadLength = view.getUint32(36, true)
  if (headerLength < 40 || headerLength + payloadLength > bytes.byteLength) throw new Error('truncated RSF1 frame')
  const flags = view.getUint8(33)
  return { sequence: Number(view.getBigUint64(8, true)), step: Number(view.getBigUint64(16, true)), width: view.getUint32(24, true), height: view.getUint32(28, true),
    format: view.getUint8(32) === 1 ? 'jpeg' : view.getUint8(32) === 2 ? 'rgba8' : 'unknown', held: (flags & 1) !== 0, video: (flags & 2) !== 0,
    payload: bytes.subarray(headerLength, headerLength + payloadLength) }
}

// The runtime's own world frame (no page UI), requested at the page's current
// frame size so the renderer is not resized. RGBA8 payloads are saved raw.
export async function worldFrame(page, directory) {
  const facts = await frameFacts(page)
  if (!facts) throw new Error('capability_unavailable: not an Engine stream page')
  const origin = new URL(page.url()).origin
  const url = `${origin}/__rusty/product/runtime/frames?after=${Math.max(0, facts.sequence - 1)}&width=${facts.width}&height=${facts.height}&cssWidth=${facts.cssWidth}`
  const response = await fetch(url, { cache: 'no-store' })
  if (response.status === 204) throw new Error('no frame arrived within the runtime wait')
  if (!response.ok) throw new Error(`frame request refused: HTTP ${response.status}`)
  const frame = parseFrame(new Uint8Array(await response.arrayBuffer()))
  const path = join(directory, `world-${frame.sequence}-step-${frame.step}.${frame.format === 'jpeg' ? 'jpg' : 'rgba'}`)
  await writeFile(path, frame.payload)
  const { payload, ...facts2 } = frame
  return { path, ...facts2, source: 'runtime frame stream; world only, no page UI' }
}

// Inspection views, overlays and scene readouts are the Engine's own
// engine.inspect.* and engine.scene.* debug commands, which the page hook
// does not carry: they run against the page's product host directly, as the
// engine backend runs them. Each argument is checked to be one word; the
// Engine decides what it means and its refusals pass through as worded.
const VIEW_ID = /^[A-Za-z0-9_-]{1,64}$/
const NAME = /^[A-Za-z0-9_.-]{1,64}$/
const FRAMINGS = ['front', 'back', 'left', 'right', 'side', 'top', 'underside', 'close']
const SCENE_LAYERS = ['scene', 'debug', 'ui', 'viewmodel', 'backdrop']
const finite = (...values) => values.every(v => typeof v === 'number' && Number.isFinite(v))
const handleWord = handle => {
  if (!Number.isSafeInteger(handle) || handle < 0) throw new Error('handle must be a scene handle from scene objects')
  return String(handle)
}

export function inspectCommand(request) {
  const { op, mode = '', id } = request
  if (id !== undefined && !VIEW_ID.test(id)) throw new Error('invalid target/action id')
  if (op === 'view') {
    const view = 'engine.inspect.view'
    if (mode === '' || mode === 'list') return `${view} list`
    if (mode === 'targets') return 'engine.inspect.target list'
    if (!id) throw new Error(`view ${mode} requires id; example: {op:"view",mode:"create",id:"side"}`)
    switch (mode) {
      case 'create':
        if (request.anchor === undefined) return `${view} create ${id}`
        if (!NAME.test(request.anchor)) throw new Error('view anchor must be one word')
        return `${view} create ${id} anchor ${request.anchor}`
      case 'close': case 'select': return `${view} ${mode} ${id}`
      case 'camera': {
        const c = request.camera
        if (!c || !Array.isArray(c.position) || c.position.length !== 3) throw new Error('view camera requires camera {position:[x,y,z],yawDegrees,pitchDegrees[,fovDegrees]}')
        const values = [...c.position, c.yawDegrees, c.pitchDegrees, ...(c.fovDegrees === undefined ? [] : [c.fovDegrees])]
        if (!finite(...values)) throw new Error('view camera must be finite')
        return `${view} camera ${id} ${values.join(' ')}`
      }
      case 'orbit': {
        const yaw = request.yaw ?? 0, pitch = request.pitch ?? 0
        if (!finite(yaw, pitch)) throw new Error('view orbit yaw/pitch must be finite degrees')
        return `${view} orbit ${id} ${yaw} ${pitch}`
      }
      case 'pan': {
        const x = request.x ?? 0, y = request.y ?? 0
        if (!finite(x, y)) throw new Error('view pan x/y must be finite')
        return `${view} pan ${id} ${x} ${y}`
      }
      case 'zoom':
        if (!finite(request.factor)) throw new Error('view zoom requires a finite factor')
        return `${view} zoom ${id} ${request.factor}`
      case 'frame': {
        if (!FRAMINGS.includes(request.framing)) throw new Error('view frame requires framing front|back|left|right|side|top|underside|close')
        if (request.handle !== undefined && request.target !== undefined) throw new Error('frame names a handle or a target, not both')
        let subject = 'camera'
        if (request.handle !== undefined) subject = handleWord(request.handle)
        else if (request.target !== undefined) {
          if (!NAME.test(request.target)) throw new Error('inspection target must be one word')
          subject = `target ${request.target}`
        }
        return `${view} frame ${id} ${request.framing} ${subject}`
      }
    }
    throw new Error(`unknown view mode "${mode}"; list|targets|create|close|select|camera|orbit|pan|zoom|frame|capture`)
  }
  if (op === 'overlay') {
    const overlay = 'engine.inspect.overlay', kind = request.kind ?? ''
    if (mode === '' || mode === 'list') {
      if (kind) throw new Error('overlay list takes no kind')
      return `${overlay} list`
    }
    if (mode !== 'show' && mode !== 'hide') throw new Error(`unknown overlay mode "${mode}"; list|show|hide`)
    const base = `${overlay} ${mode}`
    let within = ''
    if (request.view !== undefined) {
      if (kind !== 'sets' && kind !== 'set') throw new Error('overlay view applies to kind sets or set')
      if (!VIEW_ID.test(request.view)) throw new Error('overlay view must be an inspection view id or player')
      within = ` in ${request.view}`
    }
    switch (kind) {
      case '': return base
      case 'sets': return `${base} sets${within}`
      case 'set':
        if (!NAME.test(request.name ?? '')) throw new Error('overlay set requires name, one word')
        return `${base} set ${request.name}${within}`
      case 'debug-layer': case 'camera': case 'motion': return `${base} ${kind}`
      case 'joints': case 'bounds': case 'ik':
        if (request.handle === undefined) throw new Error(`overlay ${kind} requires handle from scene objects`)
        return `${base} ${kind} ${handleWord(request.handle)}`
      case 'collision': {
        let command = `${base} collision`
        if (request.x !== undefined || request.y !== undefined || request.z !== undefined) {
          if (!finite(request.x, request.y, request.z)) throw new Error('overlay collision at needs finite x/y/z')
          command += ` at ${request.x} ${request.y} ${request.z}`
        }
        if (request.distance !== undefined) {
          if (!finite(request.distance)) throw new Error('overlay collision distance must be finite metres')
          command += ` ${request.distance}`
        }
        return command
      }
    }
    throw new Error(`unknown overlay kind "${kind}"; sets|set|debug-layer|camera|motion|joints|bounds|ik|collision`)
  }
  if (op === 'scene') {
    switch (mode) {
      case '': case 'objects': {
        let command = 'engine.scene.objects'
        if (request.handle !== undefined) command += ` ${handleWord(request.handle)}`
        else if (request.depth !== undefined) command += ' *'
        if (request.depth !== undefined) {
          if (!Number.isInteger(request.depth) || request.depth < 0) throw new Error('scene depth must be 0 or more')
          command += ` ${request.depth}`
        }
        if (request.layer !== undefined) {
          if (!SCENE_LAYERS.includes(request.layer)) throw new Error('scene layer is scene|debug|ui|viewmodel|backdrop')
          command += ` --layer ${request.layer}`
        }
        if (request.page !== undefined) {
          if (!Number.isInteger(request.page) || request.page < 0) throw new Error('scene page must be 0 or more')
          command += ` --page ${request.page}`
        }
        return command
      }
      case 'object':
        if (request.handle === undefined) throw new Error('scene object requires handle; example: {op:"scene",mode:"object",handle:12}')
        return `engine.scene.object ${handleWord(request.handle)}`
      case 'composition': case 'resources': return `engine.scene.${mode}`
    }
    throw new Error(`unknown scene mode "${mode}"; objects|object|composition|resources`)
  }
  throw new Error(`unknown inspection operation "${op}"`)
}

const hostOrigin = page => new URL(page.url()).origin

// One live-debug command on the page's product host. A 422 is a refusal by
// usage or the product, worded by the Engine.
export async function engineDebug(page, command) {
  const response = await fetch(`${hostOrigin(page)}/__rusty/product/runtime/debug/execute`, { method: 'POST', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: command, cache: 'no-store' })
  const text = (await response.text()).trim()
  if (response.status === 422) throw new Error(`${command} refused: ${text}`)
  if (response.status === 503) throw new Error('product host has no serving runtime (503); re-read the binding after it returns')
  if (!response.ok) throw new Error(`${command}: HTTP ${response.status}: ${text}`)
  try { return JSON.parse(text) } catch { return { message: text } }
}

const parseHeader = value => { try { return JSON.parse(value) } catch { return value } }

// One frames/capture of an inspection view, or with no view of the product's
// composition, saved with a JSON sidecar holding its frame facts and the
// view and overlay headers.
export async function inspectionCapture(page, directory, view, request) {
  const query = new URLSearchParams({ format: 'png' })
  if (request.width > 0 && request.height > 0) { query.set('width', String(request.width)); query.set('height', String(request.height)) }
  if (view) query.set('view', view)
  if (request.overlays) query.set('overlays', '1')
  const response = await fetch(`${hostOrigin(page)}/__rusty/product/runtime/frames/capture?${query}`, { cache: 'no-store' })
  if (response.status === 503) throw new Error('product host has no serving runtime (503); re-read the binding after it returns')
  if (!response.ok) throw new Error(`frame capture: HTTP ${response.status}: ${(await response.text()).trim()}`)
  const frame = parseFrame(new Uint8Array(await response.arrayBuffer()))
  const headers = {}
  for (const name of ['X-Rusty-Frame-Cameras', 'X-Rusty-Frame-View', 'X-Rusty-Frame-Overlays']) {
    const value = response.headers.get(name)
    if (value) headers[name] = value
  }
  const stem = `${view ? `view-${view}` : 'player'}-step-${frame.step}-${randomUUID().slice(0, 8)}`
  const path = join(directory, `${stem}.png`)
  await writeFile(path, frame.payload)
  const facts = { captureSequence: frame.sequence, step: frame.step, held: frame.held, video: frame.video }
  if (headers['X-Rusty-Frame-Cameras']) facts.cameras = parseHeader(headers['X-Rusty-Frame-Cameras'])
  if (headers['X-Rusty-Frame-View']) facts.view = parseHeader(headers['X-Rusty-Frame-View'])
  if (headers['X-Rusty-Frame-Overlays']) facts.overlays = parseHeader(headers['X-Rusty-Frame-Overlays'])
  const metadataPath = join(directory, `${stem}.json`)
  const result = { path, width: frame.width, height: frame.height, step: frame.step, frame: facts, overlaysRequested: !!request.overlays,
    frameCorrelation: 'capture-step', source: view ? `runtime frames/capture?view: inspection view ${view}; world only, no page UI` : "runtime frames/capture: the product's composition; world only, no page UI",
    headers, ...(view ? { viewId: view } : {}), metadataPath }
  await writeFile(metadataPath, JSON.stringify(result, null, 2))
  return result
}

// A view capture with player also captures the product's composition, so
// both name the step they were drawn at; hold time first to make it one step.
async function view(page, directory, request) {
  if (request.mode !== 'capture') return engineDebug(page, inspectCommand(request))
  if (!request.id || !VIEW_ID.test(request.id)) throw new Error('view capture requires id; example: {op:"view",mode:"capture",id:"side",player:true,overlays:true}')
  const shot = await inspectionCapture(page, directory, request.id, request)
  if (!request.player) return shot
  let player
  try { player = await inspectionCapture(page, directory, '', request) } catch (error) { return { ...shot, playerError: String(error) } }
  const sameStep = shot.step === player.step
  return { view: shot, player, sameStep, ...(sameStep ? {} : { note: 'the view and the player view drew different steps; hold time (time mode manual) to capture both at one' }) }
}

export async function playtest(page, directory, request) {
  const { op = 'discover' } = request
  if (op === 'view') return view(page, directory, request)
  if (op === 'overlay' || op === 'scene') return engineDebug(page, inspectCommand(request))
  if (op === 'discover') {
    const answer = await engineCall(page, request)
    const commands = answer?.commands ?? []
    for (const [name, command] of [['view', 'engine.inspect.view'], ['overlay', 'engine.inspect.overlay'], ['scene', 'engine.scene.objects']]) {
      if (commands.includes(command) && Array.isArray(answer.operations) && !answer.operations.includes(name)) answer.operations.push(name)
    }
    return answer
  }
  if (op === 'act' || op === 'jump') {
    const result = op === 'jump' ? await jump(page, request) : await act(page, request)
    if (request.capture) {
      const path = join(directory, `action-${randomUUID()}.png`)
      try {
        await engineCall(page, { op: 'frame' })
        const shot = await correlatedScreenshot(page, path)
        result.capture = path; result.captureFrame = shot.frame; result.frameCorrelation = shot.frameCorrelation
        if (request.world) result.worldFrame = await worldFrame(page, directory)
      } catch (error) { result.captureError = String(error) }
    }
    return result
  }
  if (op === 'survey') return survey(page, directory, request)
  if (op === 'world-frame') return worldFrame(page, directory)
  if (op === 'record') return record(page, directory, request)
  if (op === 'observe') {
    const facts = await engineCall(page, request)
    return { ...facts, observationProvenance: { sampledAt: new Date().toISOString(), kind: 'live product query', frameCorrelation: 'not measured' } }
  }
  return engineCall(page, request)
}

async function act(page, request, afterAdvance, chunkMs = 2000, resolvedPlan) {
  const plan = resolvedPlan ?? await engineCall(page, { op: 'action', id: request.id })
  if (!plan.available) return { accepted: false, plan, reason: plan.reason }
  const ms = request.ms ?? plan.durationMs
  if (!Number.isFinite(ms) || ms <= 0 || ms > 2000) throw new Error('action duration must be in (0, 2000] ms')
  if (typeof plan.key !== 'string' || !plan.key) throw new Error('action has no physical control')
  const time = await engineCall(page, { op: 'time' })
  // Focus the Engine canvas without synthesizing a gameplay click/shot.
  if (page.locator) await page.locator('canvas').first().focus()
  const key = /^Key[A-Z]$/.test(plan.key) ? plan.key.slice(3).toLowerCase() : /^Digit[0-9]$/.test(plan.key) ? plan.key.slice(5) : plan.key === 'ControlLeft' ? 'Control' : plan.key
  const pointerButton = { Primary: 'left', Secondary: 'right', Auxiliary: 'middle' }[plan.key]
  if (pointerButton) {
    const bounds = await page.locator('canvas').first().boundingBox()
    if (!bounds) throw new Error('Engine canvas is unavailable for pointer input')
    await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2)
  }
  const press = () => pointerButton ? page.mouse.down({ button: pointerButton }) : page.keyboard.down(key)
  const release = () => pointerButton ? page.mouse.up({ button: pointerButton }) : page.keyboard.up(key)
  const heldKeys = plan.heldKeys ?? []
  if (!Array.isArray(heldKeys) || heldKeys.length > 4 || heldKeys.some(k => typeof k !== 'string')) throw new Error('invalid product held controls')
  const additional = heldKeys.filter(k => k !== plan.key).map(k => /^Key[A-Z]$/.test(k) ? k.slice(3).toLowerCase() : k)
  const before = await engineCall(page, { op: 'observe' })
  let released = false
  let advancedMs = 0
  let failure, releaseError
  try {
    for (const held of additional) await page.keyboard.down(held)
    await press()
    await engineCall(page, { op: 'flush' })
    if (time.mode === 'realtime') {
      if (!plan.hold) { await release(); released = true; await engineCall(page, { op: 'flush' }) }
      await delay(ms)
    } else {
      // Consume a pressed edge once; animation time is not a held attack key.
      const first = Math.min(ms, 1000 / time.fixedStepHz)
      const result = await engineCall(page, { op: 'advance', ms: first })
      advancedMs += result.advancedMs
      if (afterAdvance) await afterAdvance(advancedMs)
      if (!plan.hold) { await release(); released = true; await engineCall(page, { op: 'flush' }) }
      while (ms > advancedMs + 0.001) {
        const rest = await engineCall(page, { op: 'advance', ms: Math.min(chunkMs, ms - advancedMs) })
        advancedMs += rest.advancedMs
        if (afterAdvance) await afterAdvance(advancedMs)
      }
    }
  } catch (error) { failure = String(error) }
  finally {
    const releaseErrors = []
    for (const control of [...additional, ...(!released ? [key] : [])]) {
      try { if (control === key) await release(); else await page.keyboard.up(control) } catch (error) { releaseErrors.push(String(error)) }
    }
    try { await engineCall(page, { op: 'flush' }) } catch (error) { releaseErrors.push(String(error)) }
    released = releaseErrors.length === 0
    if (!released) releaseError = releaseErrors.join('; ')
  }
  let observation = {}, observationError
  try { observation = await engineCall(page, { op: 'observe' }) } catch (error) { observationError = String(error) }

  const delta = differences(before, observation)
  let focus
  try { focus = await engineCall(page, { op: 'focus' }) } catch { focus = { available: false } }
  return { accepted: !failure && !releaseError, error: failure, releaseError, observationError, delivery: failure ? 'uncertain; reobserve without replay' : 'submitted', productAcceptance: 'unavailable; inspect observed effect', delta, focus, inputPath: pointerButton ? 'physical-pointer' : 'physical-keyboard', key: plan.key, plan, requestedMs: ms, advancedMs,
    inputReleased: released, observation, handback: observation.player?.dead ? 'player-dead' : plan.hold && delta.distanceMoved === 0 ? 'no-observed-movement; inspect collision or input focus' : null }
}

async function jump(page, request) {
  const time = await engineCall(page, { op: 'time' })
  if (time.mode === 'realtime') throw new Error('jump helper requires manual or action-driven time')
  const before = await engineCall(page, { op: 'observe' })
  const query = { op: 'jump-plan', x: request.x, y: request.y, z: request.z }
  let plan = await engineCall(page, query)
  if (!plan.available) return { accepted: false, plan, reason: plan.reason }
  await engineCall(page, { op: 'look', yaw: plan.yawDeltaDegrees })
  plan = await engineCall(page, query) // resolve against the current product state
  if (!plan.available) return { accepted: false, plan, reason: plan.reason }
  const result = await act(page, { id: plan.action.id }, undefined, 100, plan.action)
  if (result.accepted && plan.settleMs > 0) {
    try { const settled = await engineCall(page, { op: 'advance', ms: plan.settleMs }); result.advancedMs += settled.advancedMs }
    catch (error) { result.accepted = false; result.error = String(error); result.delivery = 'uncertain; reobserve without replay' }
  }
  try { result.observation = await engineCall(page, { op: 'observe' }) } catch (error) { result.observationError = String(error) }
  result.delta = differences(before, result.observation)
  const player = result.observation?.player, feetY = result.observation?.axes?.floorY
  return { ...result, jumpPlan: plan, targetFeet: [request.x, request.y, request.z],
    distanceToTargetFeet: player?.position && Number.isFinite(feetY) ? Math.hypot(player.position.x-request.x, feetY-request.y, player.position.z-request.z) : null,
    grounded: player?.movement?.grounded ?? null, outcome: 'inspect actual pose; estimated input window does not guarantee landing' }
}

async function survey(page, root, request) {
  const count = request.count ?? 4
  if (count !== 4 && count !== 8) throw new Error('survey count must be 4 or 8')
  const time = await engineCall(page, { op: 'time' })
  if (time.mode === 'realtime') throw new Error('select manual or action-driven time before a frozen survey')
  const initial = await engineCall(page, { op: 'camera' })
  const directory = join(root, `survey-${randomUUID()}`)
  await mkdir(directory, { recursive: true })
  const frames = []
  let failure, restored = false
  try {
    for (let i = 0; i < count; i++) {
      const camera = { ...initial.camera, yawDegrees: initial.camera.yawDegrees + i * 360 / count }
      await engineCall(page, { op: 'camera', camera })
      const path = join(directory, `${i}.png`)
      const shot = await correlatedScreenshot(page, path)
      frames.push({ path, relativeYawDegrees: i * 360 / count, camera, frame: shot.frame, frameCorrelation: shot.frameCorrelation })
    }
  } catch (error) { failure = String(error) }
  finally {
    try { await engineCall(page, { op: 'camera', camera: initial.observer ? initial.camera : null }); restored = true }
    catch (error) { failure = `${failure ?? ''} restore: ${error}` }
  }
  let contact = null, encodingError;
  try {
    contact = join(directory, 'contact.png');
    await run('ffmpeg', ['-nostdin', '-v', 'error', '-i', join(directory, '%d.png'), '-vf', `scale=320:-1,tile=${count === 4 ? '2x2' : '4x2'}`, '-frames:v', '1', contact], { timeout: 30000 });
  } catch (error) { contact = null; encodingError = String(error) }
  const result = { directory, frames, contact, restored, error: failure, encodingError, time }
  await writeFile(join(directory, 'survey.json'), JSON.stringify(result, null, 2))
  return result
}

async function record(page, root, request) {
  const ms = request.ms ?? 1000, fps = request.fps ?? 10
  if (!Number.isInteger(ms) || ms < 100 || ms > 10000 || !Number.isInteger(fps) || fps < 1 || fps > 30) throw new Error('record requires ms 100..10000 and fps 1..30')
  const directory = join(root, `record-${randomUUID()}`)
  await mkdir(directory, { recursive: true })
  const time = await engineCall(page, { op: 'time' })
  const frames = []
  let advancedMs = 0
  const capture = async () => {
    const path = join(directory, `${String(frames.length).padStart(5, '0')}.png`)
    await engineCall(page, { op: 'frame' })
    const shot = await correlatedScreenshot(page, path)
    frames.push({ path, capturedAt: new Date().toISOString(), frame: shot.frame, frameCorrelation: shot.frameCorrelation, ...(time.mode === 'realtime' ? {} : { advancedMs }) })
  }
  await capture() // Arm before the action.
  let actionResult, failure
  try {
    if (time.mode === 'realtime') {
      let actionError
      const action = request.id ? act(page, { id: request.id, ms: Math.min(ms, 2000) }).then(r => { actionResult = r }, e => { actionError = e }) : Promise.resolve()
      const start = Date.now()
      while (Date.now() - start < ms) { await delay(1000 / fps); await capture() }
      await action
      if (actionError) throw actionError
    } else {
      if (request.id) {
        actionResult = await act(page, { id: request.id, ms: Math.min(ms, 2000) }, async elapsed => { advancedMs = elapsed; await capture() }, 1000 / fps);
        if (actionResult.error || actionResult.releaseError) throw new Error(actionResult.error ?? actionResult.releaseError);
      }
      while (advancedMs < ms - 0.001) {
        const result = await engineCall(page, { op: 'advance', ms: Math.min(1000 / fps, ms - advancedMs) })
        advancedMs += result.advancedMs; await capture()
      }
    }
  } catch (error) { failure = String(error) }
  const video = join(directory, 'clip.mp4')
  const contact = join(directory, 'contact.png')
  let encodingError
  try {
    await run('ffmpeg', ['-nostdin', '-v', 'error', '-framerate', String(fps), '-i', join(directory, '%05d.png'), '-vf', 'pad=ceil(iw/2)*2:ceil(ih/2)*2', '-c:v', 'libx264', '-pix_fmt', 'yuv420p', video], { timeout: 30000 })
    await run('ffmpeg', ['-nostdin', '-v', 'error', '-i', join(directory, '%05d.png'), '-vf', `select=not(mod(n\\,${Math.max(1, Math.ceil(frames.length / 16))})),scale=320:-1,tile=4x4`, '-frames:v', '1', contact], { timeout: 30000 })
    if (request.gif) await run('ffmpeg', ['-nostdin', '-v', 'error', '-i', video, '-vf', 'fps=10,scale=640:-1', join(directory, 'clip.gif')], { timeout: 30000 })
  } catch (error) { encodingError = String(error) }
  const result = { directory, frames, video: encodingError ? null : video, contact: encodingError ? null : contact,
    gif: request.gif && !encodingError ? join(directory, 'clip.gif') : null, time, requestedMs: ms, advancedMs, nominalFps: fps, actionResult, error: failure, encodingError }
  await writeFile(join(directory, 'record.json'), JSON.stringify(result, null, 2))
  return result
}

// Same compact before/after facts used by the earlier assistant decision view;
// absent product facts stay unavailable and never become a success claim.
function differences(before, after) {
  const a = before?.player, b = after?.player;
  if (!a || !b) return { available: false };
  const result = {};
  for (const key of ['health', 'kills']) if (Number.isFinite(a[key]) && Number.isFinite(b[key])) result[key] = b[key] - a[key];
  if (a.position && b.position) result.distanceMoved = Math.hypot(...['x', 'y', 'z'].map(axis => b.position[axis] - a.position[axis]));
  if (a.ammo && b.ammo) result.ammo = Object.fromEntries(Object.keys(b.ammo).filter(key => Number.isFinite(a.ammo[key]) && Number.isFinite(b.ammo[key])).map(key => [key, b.ammo[key] - a.ammo[key]]));
  return result;
}
