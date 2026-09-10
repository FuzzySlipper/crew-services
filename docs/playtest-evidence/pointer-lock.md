# Pointer-lock diagnostics and recovery

The Wolf native adapter can report that it asked the target to deliver native
keyboard, mouse, or controller input. It cannot read `document.pointerLockElement`,
browser focus ownership, or whether a game consumed an event. Its `pointer_lock`
diagnostic therefore reports `state: "unknown"` and `observability: "unavailable"`.
This is an adapter limitation, not evidence of a missing lock or a game failure.

An `input_delivery` receipt distinguishes these facts. `state: "target-reported"`
means the target completed each requested batch step without a target-reported
error, cancellation, cleanup error, or receipt-evidence error. It establishes
only target-reported transport delivery. `state: "unknown"` means a missing,
partial, cancelled, or failed target receipt. The accompanying `outcome` names
`cancelled`, `cleanup-uncertain`, `evidence-uncertain`, `target-error`, or
`partial-or-unreported` when applicable. Neither value establishes
pointer-lock acquisition, browser event handling, frame change, or game
consumption. `game_consumption` remains `"unknown"` in native receipts.

## Native recovery

Use ordinary native inputs and capture evidence; do not inject page events or
invent a browser probe. First inspect the current status and a stream observation.
If the intended game surface is visible, send a bounded native focus attempt:

```json
[
  {"kind":"point","x":640,"y":460,"width":1280,"height":720},
  {"kind":"wait","ms":200},
  {"kind":"click","button":1,"ms":100}
]
```

Then inspect the returned receipt and take another observation. A
`target-reported` receipt confirms that the Wolf target completed the request;
the observation supplies the visible evidence. It does not turn the unknown
pointer-lock state into `locked`.

For a practical release/reacquisition attempt, send Escape as a bounded native
hold, observe, then use the focus click again:

```json
[
  {"kind":"hold","keys":[27],"ms":100},
  {"kind":"wait","ms":200}
]
```

Every hold is neutralized by the target before its receipt completes. If a
receipt has `release_errors`, cancellation fails, or the service reports cleanup
uncertainty, stop using that session and run its normal cancel/recover or release
path before starting another input batch. Do not reuse a suspected held-input
session as proof that pointer lock was restored.

## Evidence limits

The existing Gamescope/browser-window checks establish a focused, mapped game
window at stream size. They do not expose browser pointer-lock state. A backend
that has an actual browser inspection capability may report its observed lock
state separately; its result must retain the inspection backend and must not be
presented as Wolf native input delivery. Real browser lock evidence belongs with
the browser backend's own fixture and captures.
