# Agent playtesting

Go owns local sessions, the remote Wolf input controller, and evidence. A
separate Node worker executes submitted JavaScript. CLI and MCP use the same
loopback service; DSH and Den are optional clients/integrations. This component
does not use or extend the messaging fabric.

## Agent entry point

Use `playtest games` and `playtest game show GAME` to discover the available
games and controls. `playtest start GAME` launches the remote browser, creates
the native controller, forwards the game URL through container localhost, and
attempts canvas focus using a native center-area click and Tab. That click can
be observed by the game; the returned session is not a pristine game snapshot.
It returns a session ID and screenshot path. Inspect the
screenshot: `connected` means launch/transport completed, not visual acceptance.

```sh
playtest games
playtest start rusty-space
playtest observe SESSION
playtest input SESSION --json '[{"kind":"gamepad","rt":0.5,"ms":500}]'
playtest run SESSION --file /absolute/path/trial.js --budget-ms 60000
playtest script SCRIPT
playtest cancel SESSION
playtest stop SESSION
```

CLI results are JSON. Open returned PNG paths using your harness's image tool.
`playtest mcp` exposes the same operations and returns image blocks for observe.
Both clients can inspect the same session; disconnecting a client does not stop
it. The configured pool allocates one isolated slot per tester. Do not use the old Python MCP controller while
the Go service owns its capture lock. Always stop your session when finished.

## Bounded semantic controller

Use [playtest-assist](playtest-assistant.md) for short Jev-controlled intervals
with a planner-defined goal, finite tactics, observed-fact thresholds and an
interval transcript. It reuses an owned session and the same input service.

## JavaScript API

`run` submits source and returns immediately with a script ID. Poll `script` for
phase, latest checkpoint, and evidence paths. Programs have a 60-second default
wall-clock budget, configurable from 100 ms to 120 seconds, and at most 512 API
calls. Waiting at a yield counts against that budget. Split longer evaluations
into programs, inspecting observations between them.

```js
await controller.hold({ buttons: ["back"] }, 100);
for (const pressure of [0.25, 0.75]) {
  await controller.hold({ rt: pressure }, 700);
  checkpoint(`pressure-${pressure}`, await observe());
}
const choice = await yieldToAgent("choose next action", await observe());
await keyboard.hold(choice.keys, 200);
return { finished: true };
```

| API | Behavior |
| --- | --- |
| `controller.hold(state, ms)` | Complete simultaneous Xbox state; neutral afterward |
| `keyboard.hold(keys, ms)` | Named key chord; release afterward |
| `input(steps)` | Same validated native input batches as CLI/MCP |
| `observe()` | Screenshot metadata/path; no game-state injection |
| `interaction(options = {})` | Read optional product interaction facts at the reticle, or a normalized cursor point |
| `checkpoint(label, data)` | Record progress without pausing |
| `yieldToAgent(label, data)` | Pause until `resume SESSION --json VALUE` |
| `sleep(ms)` | Cancellable wait, at most 10 seconds |
| `console.log(...)` | Record values in the program journal |

Sticks `lx/ly/rx/ry` range from -1 to 1; positive X is right, positive Y is up.
Triggers `lt/rt` range from 0 to 1. Xbox names are
`a,b,x,y,lb,rb,ls,rs,start,back,guide,up,down,left,right`.
Keyboard names include letters, digits, Enter, Escape, Tab, Space, Ctrl, Shift,
Alt, Backspace, arrows, F5, and F6. Raw input supports Windows virtual-key codes. Unknown fields are rejected;
use `buttons: ["back"]`, not `back: true`. Invalid input is rejected before
delivery and does not degrade an otherwise healthy session.

Calls are serialized, including accidentally unawaited calls, and flushed before
the program completes. Use a single controller state for simultaneous movement
and look. Separate holds release between actions; parallel promises do not create
simultaneous keyboard/controller holds. Each native batch is limited to 10 seconds,
with a further target cleanup budget. These are realtime inputs, not simulation
ticks or deterministic replay.

## Product interaction queries

`interaction` is an optional, read-only product capability. Profiles opt in with
`interaction_queries: true`; the service discovers the current Engine interaction
catalog and invokes its generated `interaction.query` or
`interaction.cursor` command over the existing live-debug HTTP transport. A query returns service metadata plus the product's
facts unchanged. It never navigates, activates, changes the camera, or issues
look input.

```sh
playtest interaction SESSION
playtest interaction SESSION --json '{"mode":"cursor","x":0.5,"y":0.5,"aspect":1.7777778}'
```

The game profile's URL origin is the product debug origin, reachable from the
service machine even when the browser runs remotely. Opt-in alone does not
establish availability: each request checks the live catalog. Backend browser
capabilities describe DOM operations separately from this service-level query.
An opt-in Wolf profile example is [interaction.example.json](../configs/playtest/interaction.example.json).
Start the Engine proving product separately and adjust its URL for your machine;
loading a profile does not provision that demo.
Responses include `facts`, exact `raw_result`, `query_id`, and `evidence_path`.
Query receipts preserve product numbers; JavaScript consumers must use the raw
JSON with a lossless parser if a product emits identities above its safe integer
range. No identity is ever sent back to activate a target by this API.

Omit options, or use `{ "mode": "reticle" }`, to query the current reticle. For
`{ "mode": "cursor" }`, provide all of `x`, `y`, and `aspect`: `x` and `y` are
normalized bottom-left coordinates in `[0,1]`, and `aspect` must be greater than
zero. Cursor coordinates are not screen pixels.

Interaction facts are semantic assistance and query evidence. They do not make a
capture fresh or demonstrate visual readiness. Use ordinary `input` to act, then
query again to observe the resulting facts. An unavailable or unknown candidate
route remains unknown; callers must not infer a route or replace it with an input
action.

## Cancellation and recovery

`cancel SESSION` kills a running worker and requests target cancellation. Target
input cleanup is independent of worker cooperation. The session remains for
inspection. A script can finish as completed, failed, cancelled, timed_out, or
cleanup_uncertain; never treat the last state as verified neutralization.

`recover SESSION` explicitly stops the old session and starts a new one for the
same game, retaining the previous session ID in the new record. This can discard
game progress. It never replays a script or uncertain gameplay input. After a
service restart, saved active sessions become interrupted and must be stopped
or recovered. The target independently expires idle leases after five minutes;
observation and input renew them. Listing status does not keep an unattended
session alive forever.

Programs, API calls/results, checkpoints, and final state are retained under the
service state directory. Captures and target action receipts retain their own
timestamps and IDs. Screenshots do not establish frame freshness. Private
Moonlight logs contain credentials and must not be copied into reports.

## Service setup

On this agent box the service is installed as the user unit
`crew-playtest.service`. The CLI is `/home/agent/.local/bin/playtest`; installed
programs/configuration are under `/home/system/crew-services/playtest/`.
The remote `den-playtest-controller.service` now runs the Go target binary.
The existing machine config retains its shared capture directory and Moonlight
installation; Python is not in the execution path. Set machine `application` to
`Wolf UI`: the launcher waits for that seed runner before creating and joining
Firefox, avoiding Wolf overwriting the joined keyboard during startup.
Browser state is kept separately at `playtest/user/WolfFirefox` on den-srv;
the older shared Wolf UI profile is preserved.

The default Firefox runner now uses **Gamescope + XWayland in kiosk mode**.
The target passes the game endpoint to the managed entrypoint as data; its Go
forwarder starts before Firefox opens the loopback URL. Startup no longer types
into the address bar or presses F11. Readiness checks the window selected by
Gamescope, Firefox's class and requested title, visible mapping, and 1280×720
size. Game/asset readiness still requires inspecting the returned observation.
After loading, click the gameplay area if the game needs a gesture to lock the
pointer; initial focus does not guarantee its input handlers were ready yet.

Build the managed image with `scripts/build-playtest-browser.sh den-srv`.
It layers `den-playtest-gamescope:local` on the existing
`den-playtest-firefox:local` image and includes the Go loopback forwarder.
In Wolf's dedicated Firefox runner, use that image and these explicit variables:
`RUN_GAMESCOPE=1`, `MOZ_ENABLE_WAYLAND=0`, `PLAYTEST_BROWSER_STARTUP=1`.
Remove `RUN_SWAY`; retain the existing device configuration. The target recognizes
the startup marker and supplies `PLAYTEST_HOST`, `PLAYTEST_PORT`, and
`PLAYTEST_URL` from the selected game profile. Do not set fixed game endpoints
in the Wolf profile. Apply runner changes only with the playtest slot idle.

The managed Firefox preference
`dom.pointer-lock.reset-to-center-from-parent.enabled=false` selects its legacy
recentering path. With Firefox 155.0.1 under Gamescope, the newer parent path
added the pre-lock cursor offset to the first mouse delta. Native pointer lock
remains enabled. This setting is scoped to this dedicated kiosk browser: the
legacy path does more recentering/IPC and does not provide the newer parent-level
capture behavior for general browser chrome. See
[Mozilla's owning change](https://bugzilla.mozilla.org/show_bug.cgi?id=1255338)
and the [live configuration evidence](/home/dev/dsh-crew/experiments/wolf-den-srv/gamescope/refinement-2026-09-08.md).

After Wolf has initialized the dedicated browser state on its first launch,
provision that profile once with
`scripts/setup-playtest-browser.sh` while no Firefox session is active. It
installs the managed [browser preferences](../configs/playtest/firefox/user.js),
preserves other preferences and the old shared profile, and suppresses first-run
and crash-restore UI. See Mozilla
[onboarding preference context](https://bugzilla.mozilla.org/show_bug.cgi?id=1976001)
and [managed startup policy](https://firefox-admin-docs.mozilla.org/reference/policies/skiptermsofuse/).

For an update, stop active sessions first, then run `scripts/install-playtest.sh`.
For a first install with Wolf, set `PLAYTEST_MACHINE_CONFIG` to its existing JSON
configuration; omit it for browser-only use. The installer preserves existing
machine configuration and game profiles. It installs the local service and browser adapter; deploying the remote target remains an explicit step.

To use DSH, load `configs/playtest/dsh.patch.yml` through its normal profile
configuration. The patch launches `playtest mcp`; it contains no game logic.

Build from this repository:

```sh
go build -o /desired/bin/playtest ./cmd/playtest
go build -o /desired/bin/playtest-service ./cmd/playtest-service
CGO_ENABLED=0 go build -o /desired/bin/playtest-target ./cmd/playtest-target
CGO_ENABLED=0 go build -o /desired/bin/playtest-forward ./cmd/playtest-forward
```

`playtest-service` requires `--games` (profile array), `--state`, and `--worker`
(scriptworker/worker.mjs). Configure Wolf with `--config` plus `--forward`,
or the browser adapter with `--browser-worker` (browser/worker.mjs), or both.
`--chromium` optionally names an installed browser executable; otherwise the
adapter uses Playwright’s installed Chromium. The service listens on
`127.0.0.1:48200` by default.
Set `PLAYTEST_URL` or `playtest --url URL` to select another loopback service.
Run it under systemd with control-group cleanup for capture and worker processes.

The remote target uses `--socket`, `--state`, `--client-id`, `--port`, and
`--video-producer-buffer-caps`. The caps are machine configuration: on den-srv
the verified Wolf UI value is
`video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}`.
The target
preserves the previous target's loopback `/command` protocol. Use the existing
rootless Docker identity/socket. No graphics daemon or unrelated container
restart is required.

Game profiles live in `configs/playtest/games.json`; machine configuration is
separate. Existing game servers must be running. This slice does not build or
restart game development servers, certify mouse aiming, implement Windows, or
restore game snapshots. Native controller input is the proving path.

## Evidence and tool choice

See the [acceptance record](playtest-evidence/README.md) and the
[unprompted evaluator trial](playtest-evidence/tool-choice.md). The evaluator
noticed scripts but chose direct controls for its short adaptive probes.

Current limitation: the final Space run launched but did not consistently react
to gameplay input. Use CraftSurvive for the verified new-runtime example; see
the acceptance record before treating Space results as game defects.

## Browser sessions and comparable captures

Profiles can set `backend: "browser"` and `environment: "service"` to run a
headless browser on the service machine. Existing profiles default to `wolf`,
whose execution environment is the configured remote Wolf target. A service
can expose either or both; unsupported backends/environments fail explicitly.
The configured pool supports multiple active sessions. Each slot owns its backend
and each browser session has an isolated ephemeral browser context. Stop your
owned session when finished; other testers can continue in their own slots.

```json
{"id":"my-web-app","backend":"browser","environment":"service",
 "url":"http://192.168.1.22:3000/","description":"Local web application",
 "controls":{},"reset":"Recover creates a fresh browser context."}
```

`den-serve` owns building and serving the demo, its port and server lifecycle.
Playtest consumes the resulting URL and does not start another repo server.
The URL must be reachable from the execution environment: `localhost` in a
browser session means the service machine, not the agent's machine; for Wolf,
use the documented profile URL forwarding. Server reachability, browser
launch and product readiness are separate facts. A loaded page is not proof
that a game has finished loading resources or accepted input.

Browser commands use the same service, CLI/MCP and supervised JS worker:

```sh
playtest browser SESSION --json '{"op":"inspect"}'
playtest browser SESSION --json '{"op":"fill","selector":"input[name=title]","value":"Example"}'
playtest browser SESSION --json '{"op":"near","x":300,"y":200,"max_distance":80}'
playtest capture SESSION --json '{"label":"before","viewpoint":{"name":"entrance"}}'
playtest capture SESSION --json '{"label":"after","compare_to":"CAPTURE_ID","viewpoint":{"name":"entrance"}}'
```

```js
await browser({op: 'fill', selector: 'input[name=title]', value: 'Example'});
await browser({op: 'click', selector: 'button[type=submit]'});
const before = await capture({label: 'submitted'});
checkpoint('Inspect this original capture', before);
```

`browser` operations are backend-specific and unavailable on Wolf. Headless
browser sessions accept finite `gamepad` input as one private virtual
`mapping: "standard"` device through `navigator.getGamepads()`, while native
controllers remain hardware owned by their browser and are left visible. The
virtual device is a browser API injection, not a native hardware emulator:
axes are normalized to `[-1,1]`, triggers are button values in `[0,1]`, and it
neutralizes after every bounded step. No silent substitution is made. Session
profiles and browser inspection describe the actual available capabilities.
Long or cancelled browser operations may terminate the browser process to
ensure abandoned input cannot continue; inspect status and recover explicitly.
Idle script completion does not destroy the browser.

For Engine interaction work, inspect the live debug catalog first, then use
`interaction.inspect` to read candidates, eligibility/rejection reasons, and
the exact `useCommand`. Keep ordinary controller/keyboard input as the normal
path. When an explicit target-ID action is required, invoke the Engine CLI's
published use command; it reaches the same product handler and keeps the same
freshness, reach, and line-of-sight checks. Existing reticle/cursor
`playtest interaction` queries remain read-only and do not activate a target.

Capture sidecars preserve original image paths. `compare_to` references a prior
`capture_id`; results compare known dimensions and supplied viewpoint metadata,
not image quality or game state. Viewpoint and assistance fields are explicitly
caller supplied. The only overlay policy currently supported is `preserve`;
product UI and diagnostics are not hidden. Unknown renderer, readiness or
stream freshness facts remain unknown. Application canvas backing dimensions
are distinct from screenshot size. GPU encoding is not proof of GPU rendering.

The final visual reviewer must inspect both original images directly, record
neutral observations before acceptance mapping, and report pass/fail/uncertainty
with image references. Keep runtime diagnostics, assisted mechanics and ordinary
control usability separate. Engine presentation/frame correlation awaits the
owning #7816 contract; Engine target queries await #7817/#7898 and crew-services
#7902. These are not currently exposed by the harness.

A complete browser profile example is in `configs/playtest/browser.example.json`;
merge the desired entries into the installed `games.json`. Installation preserves
existing profiles. For a browser-only first install, omit `PLAYTEST_MACHINE_CONFIG`;
the service will run without Wolf, and Wolf profiles report backend unavailable.

### Engine presentation observations

For an Engine product exposing the built-in `engine.renderer.presentation`
command, use `playtest capture SESSION --json '{"engine_presentation":true}'`
or `await capture({engine_presentation:true})`. A game profile can default this
on with `presentation_observations: true`; a capture can explicitly disable it.
The service queries the product's existing debug origin before taking the
original image. The sidecar's `engine_presentation` retains raw response, parsed
facts, request/completion times and a separate query evidence path. A missing
catalog, timeout or malformed response remains recorded; the generic capture
continues when its image transport works.

`engine_presentation_state` is the Engine observation's `submitted`, `pending`
or `unavailable` state. It is not whole-world readiness: `engine_readiness`,
`gpu_completion` and `frame_correlation` remain unavailable. Observation age is
time since Rust received browser feedback, not the age of the screenshot.
Actual submitted CSS/backing viewport, configured cameras, projection,
publication frontiers and known fallbacks are available in the unmodified
`engine_presentation.facts.presentation.submitted`. Hardware acceleration and
adapter identity remain unavailable unless independently observed; this contract
does not provide those facts.

Paired captures add `comparison.engine_presentation`: submitted camera, view
layout and viewport equality, plus runtime/surface agreement. Surface-local view
revisions are compared only within the same runtime and surface. Stale runtime
feedback is not usable for comparison. Pending observations retain the last
submitted snapshot, which can differ from the live/requested camera. These
comparisons never assert that a PNG corresponds to that submission.

Products own viewpoint visits and any movement they cause. Record the requested
name/pose in `viewpoint`, label that assistance, then inspect the configured
primary view's submitted camera rather than the fallback camera. Revisit and
compare actual submitted camera and viewport before making a visual judgment.
This read-only capture option does not visit viewpoints, hide diagnostics, wait
for global idleness, or certify visual acceptance. See the upstream
[presentation contract](/home/dev/rusty-engine/docs/presentation-capture.md).

## Configurable tester pool

The installed pool configuration is
`/home/system/crew-services/playtest/pool.json`, initially:

```json
{"size": 2}
```

`playtest start GAME` allocates a free slot and returns `id` and `slot_id`.
The ordinary CLI/MCP/JS commands still use the session or script ID. Separate
slots own separate Moonlight identities, Firefox state, input targets, local
capture processes and session journals. `playtest status` returns aggregate
`pool` occupancy and a `slots` array; `playtest status SESSION` stays specific
to that session. Cancelling, recovering or stopping one does not release another.

The provisioned native targets use `--wolf-state-dir` to identify their own
already-created controller from the client's private Wolf UI device records.
Firefox receives read-only binds for only that controller's event/js nodes;
a whole-host `/dev/input` bind is removed from its runner. The target refuses
launch if it cannot identify exactly one controller. `MOZ_LEGACY_PROFILES=1`
keeps Firefox on the profile prepared by `setup-playtest-browser.sh`, including
its managed first-run and pointer-lock preferences.

When full, start waits in a cancellable FIFO queue for up to 15 seconds, then
returns `pool_busy`. Optional `queue_wait_ms` in pool.json accepts 0..20000;
zero means no waiting. Queued starts are not persisted or replayed on restart.
Recovered sessions retain their slot; interrupted sessions hold it until explicit
recovery or stop. Capture sidecars include before/after `pool_activity` snapshots.
Occupancy is not a GPU-performance guarantee: an unattended game can keep rendering.

To increase capacity, change `size` in that local file and run
`scripts/configure-playtest-pool.sh --pool /home/system/crew-services/playtest/pool.json --legacy-machine /home/system/crew-services/playtest/machine.json`
from the crew-services checkout while the pool is idle, then restart
`systemctl --user restart crew-playtest.service`. Provisioning prepares each new
slot's paired client, controller unit and private browser state; there is no
source edit or rebuild to increase capacity. Slot machine configurations live
beside pool.json at `slots/slot-N/machine.json`. The normal installer preserves
pool and slot configuration and enables `--pool` when pool.json is present.

The installed Moonlight configurations use `bitrate: 20000` (Kbps) and the
adapter explicitly selects stereo audio. Current Wolf can misroute same-source-IP
clients when Moonlight uses its low-bitrate audio handshake; the 20 Mbps stereo
configuration avoids that path with our desktop Moonlight client. Keep this
setting when adding slots. See [Wolf PR #441](https://github.com/games-on-whales/wolf/pull/441)
for the upstream protocol issue.

Slot 1 retains the original session/evidence directory for historical IDs;
additional session/script directories are under the service state's
`slots/slot-N`. Paired capture baselines are local to the slot's capture store.
All slots share hardware. Different repository demos remain independently
served by den-serve; the pool does not duplicate or restart those demo servers.

The [two-slot acceptance record](playtest-evidence/pool.md) links original frames
and receipts for concurrent demos, controller isolation, cancellation, queued reuse
and recovery.

## Localhost forwarding diagnostics

The native container forwarder logs dial/copy failures with a process-local
connection ID, endpoints, elapsed time, and directional byte counts. It never
logs HTTP bodies. Normal traffic is quiet. For a diagnostic container launch,
`PLAYTEST_FORWARD_TRACE=1` enables connection/EOF records through the startup
script; the standalone binary accepts `--trace`. Retain container logs before
releasing the lease, because cleanup removes its container. These records
describe socket delivery, not whether an Engine mutation committed. Never
replay input merely because a browser fetch failed.

Engine #7920 motivated this attribution: a historical native-browser input
NetworkError was not reproduced and its cause is still unknown. The separate
Engine #7921 whole-host restart cursor issue was fixed upstream.
