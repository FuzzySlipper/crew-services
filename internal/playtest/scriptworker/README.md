# Script worker protocol

`worker.mjs` runs one trusted local JavaScript playtest script in a separate Node process. It accepts newline-delimited JSON on stdin and writes only newline-delimited JSON protocol records on stdout.

The first stdin record is `{"type":"run","source":"..."}`. The worker evaluates `source` as an async function, so a script may use `await` and a top-level `return`. Every later stdin record is the response to an outbound RPC: `{"id":"1","result":...}` or `{"id":"1","error":"..."}`.

Outbound calls have the form `{"type":"call","id":"1","method":"...","args":...}`. Calls are emitted one at a time and the next call waits for the preceding response. The Go parent owns the method implementation and may hold a `yield` response until an operator resumes the script. The terminal record is `{"type":"done","result":...}` or `{"type":"done","error":"..."}`.

The script receives `controller.hold(state, ms)`, `keyboard.hold(keys, ms)`, `input(steps)`, `observe()`, `interaction(options = {})`, `checkpoint(label, data)`, `sleep(ms)`, `yieldToAgent(label, data)`, and `console.log(...args)`. Their RPC methods and arguments are `controller` with `{state,ms}`, `keyboard` with `{keys,ms}`, `input` with `{steps}`, `observe` with `{}`, `interaction` with its options object, `checkpoint` with `{label,data}`, `sleep` with `{ms}`, `yield` with `{label,data}`, and `log` with `{args}`. `interaction` reads only the optional product interaction query; it does not activate, navigate, or look. `checkpoint` and `console.log` do not block script execution, but their calls remain in the serialized queue and finish before `done`. A failed call that script code never awaits, catches, or otherwise observes becomes a terminal error after queued calls settle.
