#!/usr/bin/env node
import { createInterface } from 'node:readline'
import { chromium } from 'playwright'

const MAX_SELECTOR = 1024
const MAX_VALUE = 8192
const MAX_DISTANCE = 256
const MAX_CONSOLE = 64
const MAX_TARGETS = 100
const TOKEN_LIFETIME_MS = 30_000
const DEFAULT_TIMEOUT = 5_000
const MAX_TIMEOUT = 15_000
const GAMEPAD_STATE_KEY = '__crewPlaytestGamepad'
const GAMEPAD_BUTTONS = Object.freeze({ a: 0, b: 1, x: 2, y: 3, lb: 4, rb: 5, lt: 6, rt: 7, back: 8, start: 9, ls: 10, rs: 11, up: 12, down: 13, left: 14, right: 15, guide: 16 })
const GAMEPAD_BUTTON_COUNT = 17

let context
let page
let config
let cursor = { x: 0, y: 0 }
let serial = Promise.resolve()
let sequence = 0
const tokens = new Map()
const consoleEvents = []
const pageErrors = []
const assistanceEvents = []

// This runs in the page before its application scripts. It augments (rather
// than replaces) the browser's Gamepad API, so any ordinary hardware gamepads
// remain visible and the virtual device occupies only a vacant slot.
const GAMEPAD_INIT_SCRIPT = `(() => {
  const key = ${JSON.stringify(GAMEPAD_STATE_KEY)};
  if (window[key]) return;
  const buttonNames = ${JSON.stringify(GAMEPAD_BUTTONS)};
  const original = typeof navigator.getGamepads === 'function' ? navigator.getGamepads.bind(navigator) : null;
  const state = { connected: false, index: 0, timestamp: 0, axes: [0, 0, 0, 0], buttons: Array(${GAMEPAD_BUTTON_COUNT}).fill(0) };
  const gamepad = {
    get id() { return 'Crew Playtest Virtual Gamepad'; },
    get index() { return state.index; },
    get connected() { return state.connected; },
    get mapping() { return 'standard'; },
    get timestamp() { return state.timestamp; },
    get axes() { return state.axes.slice(); },
    get buttons() { return state.buttons.map(value => ({ pressed: value > 0, touched: value > 0, value })); },
    get vibrationActuator() { return null; },
  };
  const tick = () => { state.timestamp = performance.now(); };
  const assignIndex = native => {
    const vacancy = native.findIndex(value => value === null || value === undefined);
    state.index = vacancy === -1 ? native.length : vacancy;
    return state.index;
  };
  const dispatch = type => {
    let event;
    try { event = new GamepadEvent(type, { gamepad }); }
    catch {
      event = new Event(type);
      Object.defineProperty(event, 'gamepad', { configurable: true, enumerable: true, value: gamepad });
    }
    window.dispatchEvent(event);
  };
  const neutralize = () => { state.axes = [0, 0, 0, 0]; state.buttons = Array(${GAMEPAD_BUTTON_COUNT}).fill(0); tick(); };
  try {
    Object.defineProperty(navigator, 'getGamepads', {
      configurable: true,
      value: () => {
        const native = original ? Array.from(original() || []) : [];
        if (!state.connected) return native;
        // Never overwrite a physical entry. Chromium's empty GamepadList may
        // contain null slots, so use its first vacancy before appending.
        native[assignIndex(native)] = gamepad;
        return native;
      },
    });
    window[key] = {
      set(input) {
        state.axes = input.axes.slice();
        state.buttons = Array(${GAMEPAD_BUTTON_COUNT}).fill(0);
        for (const name of input.buttons) state.buttons[buttonNames[name]] = 1;
        for (const [name, value] of Object.entries(input.buttonValues)) state.buttons[buttonNames[name]] = value;
        const wasConnected = state.connected;
        state.index = assignIndex(original ? Array.from(original() || []) : []);
        state.connected = true;
        tick();
        if (!wasConnected) dispatch('gamepadconnected');
      },
      neutralize,
      dispose() {
        if (!state.connected) return;
        neutralize();
        state.connected = false;
        dispatch('gamepaddisconnected');
      },
    };
  } catch (error) {
    window[key] = { error: error instanceof Error ? error.message : String(error) };
  }
})();`

function result(id, value) { process.stdout.write(`${JSON.stringify({ id, result: value })}\n`) }
function failure(id, error) { process.stdout.write(`${JSON.stringify({ id, error: error instanceof Error ? error.message : String(error) })}\n`) }

function boundedString(value, label, max = MAX_SELECTOR) {
  if (typeof value !== 'string' || value.length === 0 || value.length > max) throw new Error(`${label} must be a non-empty string up to ${max} characters`)
  return value
}

function boundedTimeout(value) {
  if (value === undefined) return DEFAULT_TIMEOUT
  if (!Number.isInteger(value) || value < 1 || value > MAX_TIMEOUT) throw new Error(`timeout_ms must be an integer from 1 through ${MAX_TIMEOUT}`)
  return value
}

function addBounded(collection, value) {
  collection.push(value)
  while (collection.length > MAX_CONSOLE) collection.shift()
}

function serializeError(error) {
  return error instanceof Error ? error.message : String(error)
}

function events() { return { console: [...consoleEvents], page_errors: [...pageErrors] } }

function recordAssistance(value) {
  assistanceEvents.push({ at: new Date().toISOString(), ...value })
  while (assistanceEvents.length > 32) assistanceEvents.shift()
}

async function currentPage() {
  if (!page || page.isClosed()) throw new Error('browser page is unavailable; release and acquire a new session')
  return page
}

async function launch(params) {
  if (context) throw new Error('browser is already launched')
  config = params
  const width = params.width
  const height = params.height
  if (!Number.isInteger(width) || width < 320 || width > 3840 || !Number.isInteger(height) || height < 240 || height > 2160) throw new Error('viewport must be within 320x240 through 3840x2160')
  const launchOptions = { headless: params.headless !== false, viewport: { width, height }, deviceScaleFactor: 1 }
  if (typeof params.executable_path === 'string' && params.executable_path.length > 0) launchOptions.executablePath = params.executable_path
  context = await chromium.launchPersistentContext(params.user_data_dir, launchOptions)
  context.setDefaultTimeout(DEFAULT_TIMEOUT)
  await context.addInitScript({ content: GAMEPAD_INIT_SCRIPT })
  page = context.pages()[0] || await context.newPage()
  page.on('console', message => addBounded(consoleEvents, { type: message.type(), text: message.text().slice(0, 4096), truncated: message.text().length > 4096, location: message.location() }))
  page.on('pageerror', error => addBounded(pageErrors, serializeError(error).slice(0, 4096)))
  return { headless: params.headless !== false, viewport: { width, height, device_pixel_ratio: 1 }, browser: 'chromium', renderer: 'unknown' }
}

async function inspect(params) {
  const active = await currentPage()
  const selector = params.selector === undefined ? undefined : boundedString(params.selector, 'selector')
  const targets = await active.evaluate(({ selector, maxTargets }) => {
    const list = selector ? Array.from(document.querySelectorAll(selector)) : Array.from(document.querySelectorAll('button,a[href],input,textarea,select,[role="button"],[role="link"],[contenteditable="true"]'))
    return list.slice(0, maxTargets).map((element, index) => {
      const rect = element.getBoundingClientRect()
      const style = getComputedStyle(element)
      const visible = rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none' && style.opacity !== '0'
      const disabled = element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true'
      return { index, tag: element.tagName.toLowerCase(), text: (element.innerText || element.getAttribute('aria-label') || element.getAttribute('title') || '').trim().slice(0, 256), id: element.id || undefined, role: element.getAttribute('role') || undefined, disabled, visible, rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height } }
    })
  }, { selector, maxTargets: MAX_TARGETS })
  const metadata = await pageMetadata(active)
  return { ...metadata, targets, ...events() }
}

async function pageMetadata(active) {
  const metadata = await active.evaluate(() => {
    const canvases = Array.from(document.querySelectorAll('canvas')).slice(0, 32).map(canvas => {
      const rect = canvas.getBoundingClientRect()
      return { css: { x: rect.x, y: rect.y, width: rect.width, height: rect.height }, backing: { width: canvas.width, height: canvas.height } }
    })
    return { title: document.title, pointer_lock: document.pointerLockElement !== null, viewport: { width: window.innerWidth, height: window.innerHeight, device_pixel_ratio: window.devicePixelRatio }, canvases }
  })
  return { url: active.url(), ...metadata, cursor: { ...cursor }, renderer: 'unknown', dom_assistance: [...assistanceEvents] }
}

async function click(params) {
  const active = await currentPage()
  if (params.selector !== undefined) {
    const selector = boundedString(params.selector, 'selector')
    await active.locator(selector).click({ timeout: boundedTimeout(params.timeout_ms) })
    return { action: 'click', selector, ...events() }
  }
  const point = pointParams(params)
  await active.mouse.click(point.x, point.y, { button: button(params.button) })
  cursor = point
  return { action: 'click', point, ...events() }
}

async function fill(params) {
  const active = await currentPage()
  const selector = boundedString(params.selector, 'selector')
  const value = typeof params.value === 'string' && params.value.length <= MAX_VALUE ? params.value : (() => { throw new Error(`value must be a string up to ${MAX_VALUE} characters`) })()
  await active.locator(selector).fill(value, { timeout: boundedTimeout(params.timeout_ms) })
  return { action: 'fill', selector, length: value.length, ...events() }
}

async function press(params) {
  const active = await currentPage()
  const key = boundedString(params.key, 'key', 128)
  if (params.selector !== undefined) await active.locator(boundedString(params.selector, 'selector')).press(key, { timeout: boundedTimeout(params.timeout_ms) })
  else await active.keyboard.press(key)
  return { action: 'press', key, ...events() }
}

function pointParams(params) {
  if (!Number.isFinite(params.x) || !Number.isFinite(params.y) || params.x < 0 || params.y < 0 || params.x > 3840 || params.y > 2160) throw new Error('x and y must be finite viewport coordinates within 0..3840 and 0..2160')
  return { x: params.x, y: params.y }
}

function button(value) {
  if (value === undefined || value === 1 || value === 'left') return 'left'
  if (value === 2 || value === 'middle') return 'middle'
  if (value === 3 || value === 'right') return 'right'
  throw new Error('button must be left, middle, right, or 1..3')
}

async function near(params) {
  const active = await currentPage()
  const point = pointParams(params)
  if (!Number.isFinite(params.max_distance) || params.max_distance < 1 || params.max_distance > MAX_DISTANCE) throw new Error(`max_distance must be a finite number from 1 through ${MAX_DISTANCE}`)
  const evaluation = await active.evaluate(({ x, y, maxDistance, maxTargets }) => {
    const selectors = 'button,a[href],input,textarea,select,[role="button"],[role="link"],[contenteditable="true"],[tabindex]:not([tabindex="-1"])'
    const viewport = { width: window.innerWidth, height: window.innerHeight }
    const distance = (rect) => Math.hypot(Math.max(rect.left - x, 0, x - rect.right), Math.max(rect.top - y, 0, y - rect.bottom))
    const values = []
    const walker = document.createTreeWalker(document.documentElement, NodeFilter.SHOW_ELEMENT)
    let element
    let scanned = 0
    let truncated = false
    while ((element = walker.nextNode())) {
      if (!element.matches(selectors)) continue
      if (scanned++ >= maxTargets) { truncated = true; break }
      const rect = element.getBoundingClientRect()
      const style = getComputedStyle(element)
      if (rect.width <= 0 || rect.height <= 0 || style.visibility === 'hidden' || style.display === 'none' || style.opacity === '0') continue
      const clipped = { left: Math.max(0, rect.left), top: Math.max(0, rect.top), right: Math.min(viewport.width, rect.right), bottom: Math.min(viewport.height, rect.bottom) }
      if (clipped.right <= clipped.left || clipped.bottom <= clipped.top) continue
      const d = distance(clipped)
      if (d > maxDistance) continue
      const point = { x: Math.min(Math.max(x, clipped.left + 0.5), clipped.right - 0.5), y: Math.min(Math.max(y, clipped.top + 0.5), clipped.bottom - 0.5) }
      const hit = document.elementFromPoint(point.x, point.y)
      const disabled = element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true'
      values.push({ nodeIndex: scanned - 1, element, d, point, rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height }, hit: Boolean(hit && (hit === element || element.contains(hit))), disabled, tag: element.tagName.toLowerCase(), text: (element.innerText || element.getAttribute('aria-label') || element.getAttribute('title') || '').trim().slice(0, 256), id: element.id || undefined, role: element.getAttribute('role') || undefined })
    }
    values.sort((a, b) => a.d - b.d)
    return { candidates: values.map(value => ({ node_index: value.nodeIndex, d: value.d, point: value.point, rect: value.rect, hit: value.hit, disabled: value.disabled, tag: value.tag, text: value.text, id: value.id, role: value.role })), scan: { inspected: scanned, truncated } }
  }, { x: point.x, y: point.y, maxDistance: params.max_distance, maxTargets: MAX_TARGETS })
  cursor = point
  if (evaluation.candidates.length === 0) return { outcome: 'no_candidate', point, max_distance: params.max_distance, candidate_scan: evaluation.scan }
  const candidate = evaluation.candidates[0]
  if (candidate.disabled) return { outcome: 'disabled', point, candidate, candidate_scan: evaluation.scan }
  if (!candidate.hit) return { outcome: 'obstructed', point, candidate, candidate_scan: evaluation.scan }
  if (evaluation.candidates.length > 1 && Math.abs(evaluation.candidates[1].d - candidate.d) < 2) return { outcome: 'ambiguous', point, candidates: evaluation.candidates.slice(0, 2), candidate_scan: evaluation.scan }
  const handle = await active.locator('button,a[href],input,textarea,select,[role="button"],[role="link"],[contenteditable="true"],[tabindex]:not([tabindex="-1"])').nth(candidate.node_index).elementHandle()
  if (!handle) return { outcome: 'stale', reason: 'candidate_detached_before_token', candidate_scan: evaluation.scan }
  const token = `near-${++sequence}`
  tokens.set(token, { expires: Date.now() + TOKEN_LIFETIME_MS, point, candidate, handle })
  while (tokens.size > 100) { const oldest = tokens.entries().next().value; tokens.delete(oldest[0]); void oldest[1].handle.dispose() }
  return { outcome: 'ok', point, max_distance: params.max_distance, token, candidate, expires_in_ms: TOKEN_LIFETIME_MS, candidate_scan: evaluation.scan }
}

async function select(params) {
  const active = await currentPage()
  const token = boundedString(params.token, 'token', 128)
  const saved = tokens.get(token)
  tokens.delete(token)
  if (!saved || saved.expires < Date.now()) { if (saved) await saved.handle.dispose(); return { outcome: 'stale', reason: 'unknown_or_expired_token' } }
  try {
    const action = params.action === undefined ? 'move' : params.action
    if (action !== 'click' && action !== 'move') throw new Error('action must be click or move')
    const checked = await saved.handle.evaluate((element, { point, expected }) => {
      if (!element.isConnected) return { outcome: 'stale', reason: 'candidate_detached' }
      const hit = document.elementFromPoint(point.x, point.y)
      if (!hit || !(hit === element || element.contains(hit))) return { outcome: 'obstructed' }
      const rect = element.getBoundingClientRect()
      const same = Math.abs(rect.x - expected.rect.x) < 2 && Math.abs(rect.y - expected.rect.y) < 2 && Math.abs(rect.width - expected.rect.width) < 2 && Math.abs(rect.height - expected.rect.height) < 2
      if (!same) return { outcome: 'stale', rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height } }
      if (element.matches(':disabled') || element.getAttribute('aria-disabled') === 'true') return { outcome: 'disabled' }
      return { outcome: 'ok' }
    }, { point: saved.candidate.point, expected: saved.candidate })
    if (checked.outcome !== 'ok') return checked
    await active.mouse.move(saved.candidate.point.x, saved.candidate.point.y)
    cursor = saved.candidate.point
    if (action === 'click') {
      await saved.handle.click({ position: { x: cursor.x - saved.candidate.rect.x, y: cursor.y - saved.candidate.rect.y }, timeout: DEFAULT_TIMEOUT })
    }
    const receipt = { outcome: 'ok', action, token, point: cursor, assistance: 'near_cursor_dom', candidate: saved.candidate }
    recordAssistance(receipt)
    return receipt
  } finally {
    await saved.handle.dispose()
  }
}

async function browser(params) {
  if (!params || typeof params !== 'object' || Array.isArray(params)) throw new Error('browser request must be an object')
  switch (params.op) {
    case 'inspect': return inspect(params)
    case 'click': return click(params)
    case 'fill': return fill(params)
    case 'press': return press(params)
    case 'near': return near(params)
    case 'select': return select(params)
    default: throw new Error('browser op must be inspect, click, fill, press, near, or select')
  }
}

async function input(params) {
  const active = await currentPage()
  if (!Array.isArray(params.steps) || params.steps.length > 128) throw new Error('steps must contain at most 128 items')
  const checked = params.steps.map(validateInputStep)
  for (const step of checked) {
    switch (step.kind) {
      case 'wait':
        if (!Number.isInteger(step.ms) || step.ms < 0 || step.ms > 10_000) throw new Error('wait ms must be 0..10000')
        await new Promise(resolve => setTimeout(resolve, step.ms)); break
      case 'hold': {
        const keys = step.keys
        for (const key of keys) await active.keyboard.down(key)
        try { await new Promise(resolve => setTimeout(resolve, step.ms)) } finally { for (const key of [...keys].reverse()) await active.keyboard.up(key) }
        break
      }
      case 'move':
        throw new Error('capability_unavailable: relative mouse move is not supported by a browser page; use point for absolute coordinates')
      case 'point':
        cursor = pointParams(step)
        await active.mouse.move(cursor.x, cursor.y); break
      case 'click':
        await active.mouse.down({ button: step.button })
        try { if (step.ms > 0) await new Promise(resolve => setTimeout(resolve, step.ms)) } finally { await active.mouse.up({ button: step.button }) }
        break
      case 'gamepad':
        await setGamepad(active, step)
        try { if (step.ms > 0) await new Promise(resolve => setTimeout(resolve, step.ms)) } finally { await neutralizeGamepad(active) }
        break
      default: throw new Error('input kind must be hold, move, point, click, wait, or gamepad')
    }
  }
  return { completed_steps: params.steps.length, cursor: { ...cursor }, ...events() }
}

function validateInputStep(step) {
  if (!step || typeof step !== 'object' || Array.isArray(step)) throw new Error('input step must be an object')
  switch (step.kind) {
    case 'wait': if (!Number.isInteger(step.ms) || step.ms < 0 || step.ms > 10_000) throw new Error('wait ms must be 0..10000'); return step
    case 'hold': if (!Array.isArray(step.keys) || step.keys.length < 1 || step.keys.length > 8 || !Number.isInteger(step.ms) || step.ms < 0 || step.ms > 10_000) throw new Error('hold requires 1..8 keys and ms 0..10000'); return { ...step, keys: step.keys.map(playwrightKey) }
    case 'move': if (!Number.isFinite(step.dx) || !Number.isFinite(step.dy)) throw new Error('move requires finite dx and dy'); throw new Error('capability_unavailable: relative mouse move is not supported by a browser page; use point for absolute coordinates')
    case 'point': return { ...step, ...pointParams(step) }
    case 'click': if (!Number.isInteger(step.ms) || step.ms < 0 || step.ms > 10_000) throw new Error('click ms must be 0..10000'); return { ...step, button: button(step.button) }
    case 'gamepad': return gamepadStep(step)
    default: throw new Error('input kind must be hold, move, point, click, wait, or gamepad')
  }
}

function gamepadStep(step) {
  const allowed = new Set(['kind', 'ms', 'buttons', 'lx', 'ly', 'rx', 'ry', 'lt', 'rt'])
  for (const name of Object.keys(step)) {
    if (allowed.has(name)) continue
    if (name === 'back') throw new Error("unknown gamepad field back; use buttons:['back']")
    throw new Error(`unknown gamepad input field ${JSON.stringify(name)}`)
  }
  const ms = step.ms === undefined ? 0 : step.ms
  if (!Number.isInteger(ms) || ms < 0 || ms > 10_000) throw new Error('gamepad ms must be 0..10000')
  const axis = name => {
    const value = step[name] === undefined ? 0 : step[name]
    if (!Number.isFinite(value) || value < -1 || value > 1) throw new Error(`${name} must be finite in [-1, 1]`)
    return value
  }
  const trigger = name => {
    const value = step[name] === undefined ? 0 : step[name]
    if (!Number.isFinite(value) || value < 0 || value > 1) throw new Error(`${name} must be finite in [0, 1]`)
    return value
  }
  const buttons = step.buttons === undefined ? [] : step.buttons
  if (!Array.isArray(buttons)) throw new Error('buttons must be an array of Xbox button names')
  for (const name of buttons) if (typeof name !== 'string' || GAMEPAD_BUTTONS[name] === undefined) throw new Error('Unknown Xbox button')
  return { kind: 'gamepad', ms, axes: [axis('lx'), axis('ly'), axis('rx'), axis('ry')], buttons: [...new Set(buttons)], triggers: [trigger('lt'), trigger('rt')] }
}

async function setGamepad(active, step) {
  await active.evaluate(({ key, state }) => {
    const controller = window[key]
    if (!controller || controller.error) throw new Error(`capability_unavailable: browser Gamepad API injection failed${controller?.error ? `: ${controller.error}` : ''}`)
    controller.set({ axes: state.axes, buttons: state.buttons, buttonValues: { lt: state.triggers[0], rt: state.triggers[1] } })
  }, { key: GAMEPAD_STATE_KEY, state: step })
}

async function neutralizeGamepad(active, dispose = false) {
  await active.evaluate(({ key, dispose }) => {
    const controller = window[key]
    if (!controller || controller.error) return
    if (dispose) controller.dispose()
    else controller.neutralize()
  }, { key: GAMEPAD_STATE_KEY, dispose })
}

function playwrightKey(value) {
  if (typeof value === 'string' && value.length > 0 && value.length <= 128) return value
  if (!Number.isInteger(value)) throw new Error('hold keys must be named keys or virtual-key integers')
  const named = { 8: 'Backspace', 9: 'Tab', 13: 'Enter', 16: 'Shift', 17: 'Control', 18: 'Alt', 27: 'Escape', 32: 'Space', 37: 'ArrowLeft', 38: 'ArrowUp', 39: 'ArrowRight', 40: 'ArrowDown', 116: 'F5', 117: 'F6' }
  if (named[value]) return named[value]
  if ((value >= 48 && value <= 57) || (value >= 65 && value <= 90)) return String.fromCharCode(value)
  throw new Error(`unsupported virtual key ${value}`)
}

async function close() {
  if (!context) return { released: true, browser_closed: false }
  const current = context
  const active = page
  context = undefined
  page = undefined
  if (active && !active.isClosed()) {
    try { await neutralizeGamepad(active, true) } catch { /* context shutdown still removes the lease */ }
  }
  await current.close()
  return { released: true, browser_closed: true }
}

async function dispatch(message) {
  switch (message.method) {
    case 'launch': return launch(message.params || {})
    case 'navigate': { const active = await currentPage(); const url = boundedString(message.params?.url, 'url', 4096); const response = await active.goto(url, { waitUntil: 'domcontentloaded', timeout: boundedTimeout(message.params?.timeout_ms) }); if (response && !response.ok()) throw new Error(`server returned HTTP ${response.status()} for ${url}`); return { url: active.url(), ...await pageMetadata(active), ...events() } }
    case 'observe': { const active = await currentPage(); const metadata = await pageMetadata(active); const screenshot = await active.screenshot({ type: 'png' }); return { ...metadata, screenshot_base64: screenshot.toString('base64'), ...events() } }
    case 'browser': return browser(message.params)
    case 'input': return input(message.params || {})
    case 'status': return context ? { lease_state: 'active', page_open: Boolean(page && !page.isClosed()), ...await pageMetadata(await currentPage()), ...events() } : { lease_state: 'inactive' }
    case 'release': return close()
    default: throw new Error('unknown browser RPC method')
  }
}

const lines = createInterface({ input: process.stdin, crlfDelay: Infinity })
lines.on('line', line => {
  let message
  try { message = JSON.parse(line) } catch { return failure('', 'browser RPC requires JSON lines') }
  if (!Number.isInteger(message.id) || typeof message.method !== 'string') return failure(message?.id ?? '', 'browser RPC requires integer id and method')
  serial = serial.then(() => dispatch(message)).then(value => result(message.id, value), error => failure(message.id, error))
})
lines.on('close', () => { void close().finally(() => process.exit(0)) })
process.on('SIGTERM', () => { void close().finally(() => process.exit(0)) })
