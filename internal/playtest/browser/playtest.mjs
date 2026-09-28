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

export async function playtest(page, directory, request) {
  const { op = 'discover' } = request
  if (op === 'act' || op === 'jump') {
    const result = op === 'jump' ? await jump(page, request) : await act(page, request)
    if (request.capture) {
      const path = join(directory, `action-${randomUUID()}.png`)
      try { await engineCall(page, { op: 'frame' }); await page.screenshot({ path, type: 'png' }); result.capture = path } catch (error) { result.captureError = String(error) }
    }
    return result
  }
  if (op === 'survey') return survey(page, directory, request)
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
      await page.screenshot({ path, type: 'png' })
      frames.push({ path, relativeYawDegrees: i * 360 / count, camera })
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
    await engineCall(page, { op: 'frame' }); await page.screenshot({ path, type: 'png' }); frames.push({ path, capturedAt: new Date().toISOString(), ...(time.mode === 'realtime' ? {} : { advancedMs }) })
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
