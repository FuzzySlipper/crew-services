#!/usr/bin/env node

import { createInterface } from 'node:readline'

let started = false
let finished = false
let nextCallID = 1
let callTail = Promise.resolve()
const pending = new Map()
const calls = []
let lines

/** Write exactly one protocol record to stdout. */
function write(record) {
  process.stdout.write(`${JSON.stringify(record)}\n`)
}

/** Turn arbitrary thrown values into a useful terminal error string. */
function errorText(error) {
  if (error instanceof Error) return error.stack || error.message
  return String(error)
}

/** Finish the worker once. */
function done(result, error) {
  if (finished) return
  finished = true
  if (error !== undefined) {
    write({ type: 'done', error: errorText(error) })
    lines?.close()
    process.stdin.pause()
    return
  }
  const record = { type: 'done' }
  if (result !== undefined) record.result = result
  write(record)
  lines?.close()
  process.stdin.pause()
}

/**
 * Return a Promise-like value that records whether script code observed it.
 * Await, then, catch, and finally all count as observation.
 */
function observable(promise, record) {
  return {
    then(onFulfilled, onRejected) {
      record.observed = true
      return promise.then(onFulfilled, onRejected)
    },
    catch(onRejected) {
      record.observed = true
      return promise.catch(onRejected)
    },
    finally(onFinally) {
      record.observed = true
      return promise.finally(onFinally)
    },
  }
}

/**
 * Queue one RPC. A later call is not emitted until this call receives its
 * response, including an error response.
 */
function call(method, args) {
  const record = { method, observed: false, settled: false, failure: undefined }
  const response = new Promise((resolve, reject) => {
    record.resolve = resolve
    record.reject = reject
  })
  const operation = callTail.then(() => {
    if (finished) throw new Error('script worker has already finished')
    const id = String(nextCallID++)
    record.id = id
    pending.set(id, record)
    write({ type: 'call', id, method, args })
    return response
  })
  record.operation = operation
  calls.push(record)
  // Keep the serialization chain alive after an RPC failure.
  callTail = operation.then(() => undefined, () => undefined)
  // This handler also suppresses Node's process-level unhandled rejection
  // reporting. We produce a deterministic terminal protocol record instead.
  operation.then(
    () => { record.settled = true },
    error => {
      record.settled = true
      record.failure = error
    },
  )
  return observable(operation, record)
}

function api() {
  return {
    controller: {
      hold: (state, ms) => call('controller', { state, ms }),
    },
    keyboard: {
      hold: (keys, ms) => call('keyboard', { keys, ms }),
    },
    input: steps => call('input', { steps }),
    observe: () => call('observe', {}),
    interaction: (options = {}) => call('interaction', options),
    browser: options => call('browser', options),
    capture: (options = {}) => call('capture', options),
    // Checkpoints are deliberately asynchronous notifications. They still
    // occupy the RPC queue and are flushed before the terminal record.
    checkpoint: (label, data) => { void call('checkpoint', { label, data }) },
    sleep: ms => call('sleep', { ms }),
    yieldToAgent: (label, data) => call('yield', { label, data }),
    console: {
      log: (...args) => { void call('log', { args }) },
    },
  }
}

async function run(source) {
  const exposed = api()
  let result
  let executionError
  try {
    const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor
    const execute = new AsyncFunction(
      'controller', 'keyboard', 'input', 'observe', 'interaction', 'checkpoint', 'sleep', 'yieldToAgent', 'console', 'browser', 'capture',
      source,
    )
    result = await execute(
      exposed.controller,
      exposed.keyboard,
      exposed.input,
      exposed.observe,
      exposed.interaction,
      exposed.checkpoint,
      exposed.sleep,
      exposed.yieldToAgent,
      exposed.console,
      exposed.browser,
      exposed.capture,
    )
  } catch (error) {
    executionError = error
  }

  await Promise.allSettled(calls.map(record => record.operation))
  if (executionError !== undefined) {
    done(undefined, executionError)
    return
  }
  const unobservedFailure = calls.find(record => record.failure !== undefined && !record.observed)
  if (unobservedFailure) {
    done(undefined, new Error(`unawaited ${unobservedFailure.method} RPC failed: ${errorText(unobservedFailure.failure)}`))
    return
  }
  done(result)
}

function acceptResponse(message) {
  if (typeof message.id !== 'string') {
    done(undefined, new Error('RPC response requires a string id'))
    return
  }
  const record = pending.get(message.id)
  if (!record) {
    done(undefined, new Error(`unexpected RPC response id ${message.id}`))
    return
  }
  pending.delete(message.id)
  if (message.error !== undefined && message.error !== null && message.error !== '') {
    record.reject(new Error(String(message.error)))
    return
  }
  record.resolve(message.result)
}

lines = createInterface({ input: process.stdin, crlfDelay: Infinity })
lines.on('line', line => {
  if (finished) return
  let message
  try {
    message = JSON.parse(line)
  } catch {
    done(undefined, new Error('stdin must contain JSONL records'))
    return
  }
  if (!started) {
    if (message?.type !== 'run' || typeof message.source !== 'string') {
      done(undefined, new Error('first record must be {type:"run",source:string}'))
      return
    }
    started = true
    void run(message.source)
    return
  }
  acceptResponse(message)
})

lines.on('close', () => {
  if (!started && !finished) done(undefined, new Error('stdin closed before run request'))
})
