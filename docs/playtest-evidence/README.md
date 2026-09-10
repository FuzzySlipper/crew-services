# Go/JS playtest acceptance, 2026-09-08

The installed CLI and MCP share the Go session service. These checks distinguish
native gameplay evidence from protocol success and focused unit coverage.

For native pointer input, see [pointer-lock diagnostics and recovery](pointer-lock.md).
Wolf receipts separate target-reported transport delivery from browser lock and
game consumption; the native adapter reports those latter facts as unknown.

- [CraftSurvive script](craft/source.js) and [journal](craft/events.jsonl): native
  controller look/movement followed by keyboard W. Compare the
  [focused baseline](craft/focused-baseline.png),
  [controller result](craft/controller-look-move.png), and
  [keyboard result](craft/keyboard-forward.png). Browser focus was prepared
  through native input; no game state was injected.
- Final [fresh CraftSurvive launch](craft-fresh/start.json) followed by a
  [plain gameplay script](craft-fresh/source.js) required no manual focus repair.
  Compare [baseline](craft-fresh/baseline.png), [controller](craft-fresh/controller.png),
  and [keyboard](craft-fresh/keyboard.png).
- Killing the local service and capture process group left the saved session
  [interrupted](service-interrupted.json). Explicit recovery produced a
  [new connected session](service-recovered.json) linked to the old ID, without
  replaying its script. The recovered browser reached the initial game scene;
  this does not imply game assets had finished loading.
- [Held controller](held.json), [cancel receipt](cancel.json), and
  [neutral device](neutral.json): cancellation interrupted an actual held Xbox
  action and Linux device readback showed all axes/buttons neutral.
- [Infinite-loop timeout](loop-timeout.json): an uncooperative JS loop was killed
  and the session remained usable. [Invalid input](invalid-input.txt) was
  rejected before delivery; [status](after-invalid-status.json) remained connected.
- [Wolf fault](launch-fault.json): a real upstream producer failure interrupted
  create and cleanup. The service retained degraded state instead of claiming
  success, and target recovery subsequently cleared its expired lease. This is
  evidence of fault reporting, not a claim that Wolf is fault-free.
- Space's final [fresh session](space-fresh/start.json) launched, but its
  [script](space-fresh/source.js) did not visibly change heading/speed. Restarting
  the demo and reloading briefly produced visible keyboard thrust; the later
  [follow-up](space-runtime-followup/script.json) again showed no reliable
  controller movement. This final Space sequence is **not a gameplay pass**.
  A [no-reset control](space-no-reset/script.json) also failed to establish
  movement. [Diagnostics](space-runtime-followup/diagnostics.json) record a
  browser-local input NetworkError and degraded browser host at 05:47:58.625Z,
  during the earlier live sequence and about 68 seconds before teardown. The
  simulation remained running. The failing event lacks an attachment ID, so
  stale-page versus current-attachment causality is unresolved; the evidence
  does not identify the forwarder, browser, or Engine as the root cause.
- Final [target status](final-status.json) has no lease, stream, or lobby;
  [Linux device readback](final-devices.json) has no remaining virtual controller.
- [Unprompted tool-choice trial](tool-choice.md): the independent evaluator
  chose direct controls rather than scripting. Its report and the invalidated
  reset finding remain visible.

The JS yield/resume trial was script `8f756861-25cd-4f7f-8f6d-24a024c34716`:
phase yielded at `next-move`, resumed with `{keys:["D"]}`, then completed.
That initial trial did not establish gameplay movement because focus was wrong;
the later linked CraftSurvive sequence does.

Focused Go race tests cover session cancellation, termination, recovery state,
input validation, target packets and cleanup, and CLI/MCP concurrency. Node
worker tests cover call ordering, error propagation, and yield. The real DSH
Cordis/MCP check discovers all 12 tools and reads target status without acquiring
a session. These are bounded checks, not certification of arbitrary games,
Windows targets, mouse aiming, frame freshness, or long unattended campaigns.

## Configurable pool

See [two-slot pool acceptance](pool.md) for concurrent Space/CraftSurvive evidence,
controller isolation, queued slot reuse and recovery.
