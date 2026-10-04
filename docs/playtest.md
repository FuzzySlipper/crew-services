# Agent playtesting

- [Tester prompts and exit interviews](playtest-agent-prompts.md)
- [Adding playtest support to another Engine product](playtest-product-integration.md)

## Adaptive Engine playtesting

`playtest assist SESSION --json '{"op":"discover"}'` discovers the Engine
inspection adapter and the product's live `playtest.help` provider. These are
ordinary CLI/MCP tools; they do not require Jev, OpenRouter, or an encounter script.
Use `playtest game show PROFILE` for reset/shared-host semantics. A browser context
is isolated; the product host's simulation can still be shared.

```sh
playtest assist SESSION --json '{"op":"time","mode":"action-driven"}'
playtest assist SESSION --json '{"op":"observe"}'
playtest assist SESSION --json '{"op":"targets"}'
playtest assist SESSION --json '{"op":"route","id":"door-north-wing"}'
playtest assist SESSION --json '{"op":"act","id":"forward","ms":200,"capture":true}'
playtest assist SESSION --json '{"op":"look","yaw":45}'
playtest assist SESSION --json '{"op":"action","id":"attack"}'
playtest assist SESSION --json '{"op":"act","id":"attack"}'
```

Through MCP, `assist` answers are compact: objects nested more than three
levels deep, lists past four entries and long strings are summarized. The
service stores every assist result whole at the `receipt` path it returns, and
`"detail":"full"` in the request returns it inline. The CLI prints the full
result.

Read observations and choose the next action. Routes/suggestions are read-only
product guidance, refreshed on request. A route reaching its goal does not mean
the player arrived; a required door action does not mean it is in reach.
Doom publishes current bindings, pose, health/ammo/cooldown, alive/hostile actors,
door/pickup state, focus and ordinary-use guidance. Missing product capabilities
are reported as unavailable. Observations include the product's generation/step.
Captures name the runtime frame and step they show; see
[captures and frame correlation](#captures-and-frame-correlation).

### Actions and time

- `time`: read, or set `mode` to `realtime`, `manual`, or `action-driven`.
- `act`: query the current product action plan, focus the Engine canvas without a
  gameplay click, press its ordinary physical key or pointer button, release it, and return fresh
  observations/deltas. `ms` overrides the live duration; range `(0,2000]`.
  Both held modes advance automatically for this bounded convenience action.
  Tap actions release after their first step; movement holds for the window.
- `action`: inspect live duration, equipment, control and availability without acting.
- `look`: relative yaw/pitch degrees through product look rules; no time advancement.
- `advance`: explicit forward `ms` in `(0,2000]`, rounded up to fixed steps;
  response reports actual advancement. No rewind or wall-time catch-up.
- Raw `input` remains ordinary realtime input. In held modes use `act`, or
  explicitly advance after submitting input. The agent chooses when to act.

An accepted tool result reports input submission, not a confirmed hit, opened door,
or mapped-intent acceptance. Inspect `delta`, `observation`, `focus`, `handback`,
and `inputReleased`. On uncertain delivery, reobserve; do not replay automatically.
Failures preserve confirmed advancement and release status. Session cancellation
closes its browser; it never repeats uncertain actions.

### Triggered spatial inspection and jump guidance

Products can expose Engine `spatial.grid`, `spatial.probe` and
`playtest.jump-plan` through the ordinary assist surface. `discover.operations`
lists callable assist names; `nativeCommands` lists the separate debug catalog.

```sh
playtest interaction SESSION
playtest assist SESSION --json '{"op":"interaction"}'
playtest assist SESSION --json '{"op":"grid","radius":4,"verticalRadius":4,"cellSize":0.25}'
playtest assist SESSION --json '{"op":"probe","distance":2}'
playtest assist SESSION --json '{"op":"clearance","x":2,"y":0,"z":-3}'
playtest assist SESSION --json '{"op":"jump-plan","x":2,"y":0,"z":-3}'
playtest assist SESSION --json '{"op":"jump","x":2,"y":0,"z":-3}'
```

Inspection does not advance time. The grid is explicitly requested, with one row
string per Z row in each Y slice, characters increasing along X. It includes its
world origin and cell size. `#` means static collision, `D` supplied dynamic
collision, `B` both and `.` no collision in those sources. Outside the queried
volume is unknown; empty cells do not establish walkability. Dimensions are
bounded to 31 cells per axis and 8192 total. Products select the supplied dynamic
colliders; consult their documentation for coverage.

Probes sample horizontal rays at ankle, step and head heights plus nearby floor
rays. They help explain bumps, without proving capsule clearance. Detailed
interaction facts include signed yaw/pitch adjustments to each candidate point,
as well as reach, occlusion and focus refusal. Looking toward a point does not
bypass those constraints.

`clearance` takes world XYZ target feet within eight units. It uses the current
body size to report current overlap, direct capsule sweep contact, target overlap
and support below the target. A contact at the start needs interpretation: check its normal/source,
since ordinary floor contact can be reported there. The query does not
automatically slide or step around it. A clear straight sweep or
suitable support normal alone does not guarantee walking or landing there.
Like grid/probe, this query leaves time held.

`jump-plan` queries current product controller tuning and grounded state.
`jump` requires held time, turns toward target feet, pulses jump, holds forward
for the estimated window, releases controls and advances a short settling window.
It returns actual pose, grounded state and distance from the requested target.
It can collide or miss; inspect the result before choosing another action.

### Drawing and observer camera

`drawing` selects `continuous` (full-rate drawing) or `on-demand`. This is independent
of time mode; input polling and native simulation keep running. `frame` requests a
current draw and returns renderer diagnostics. Follow with ordinary `observe SESSION`
for an original screenshot. The observer is a renderer-only camera and never moves
the player or changes gameplay aim/collision.

```sh
playtest assist SESSION --json '{"op":"drawing","mode":"on-demand"}'
playtest assist SESSION --json '{"op":"advance","ms":500}'
playtest assist SESSION --json '{"op":"frame"}'
playtest assist SESSION --json '{"op":"camera","move":[0,3,0],"lookAt":[0,0,0]}'
playtest assist SESSION --json '{"op":"camera","orbit":{"target":[0,0,0],"yaw":45}}'
playtest assist SESSION --json '{"op":"camera","camera":null}'
```

`camera` without arguments reads pose and whether an observer override is active.
It also accepts an absolute `camera` object with `position:[x,y,z]`, `yawDegrees`,
`pitchDegrees`, or relative `yaw`/`pitch`. Null restores the current gameplay camera.
Camera/look inspection can request a draw while normal drawing is suspended.

### Surveys and recordings

```sh
playtest assist SESSION --json '{"op":"survey","count":8}'
playtest assist SESSION --json '{"op":"record","id":"attack","ms":800,"fps":10,"gif":true}'
```

Surveys require held time. They save 4 cardinal or 8 cardinal/diagonal original
PNGs plus a contact sheet and manifest, then restore the prior camera override.
Check `restored`, including after a partial failure. This is observer evidence,
not proof of player aim.

Recordings arm a first screenshot before the optional action, retain every original
frame with the runtime frame/step it shows when known, and generate MP4/contact
sheet/optional GIF with `ffmpeg` from PATH. In held
mode they advance and capture in short windows. In realtime they sample wall time;
PNG capture can lower effective FPS. `nominalFps` describes encoding cadence, not a
claim that every game frame was captured. Frames carry capture times and held-mode
advancement. Inspect any returned original frame directly; no replay is involved.
A named held action runs for at most 2000 ms, then remaining recording time advances
with neutral input. Encoder failure preserves originals and its diagnostic.


## Captures and frame correlation

Engine products render in their own runtime process on the local GPU and stream
frames to the page. The page shows each frame on a canvas that names its runtime
frame sequence and simulation step. Every screenshot reads that name before and
after it is taken:

- `frameCorrelation: "frame-sequence"` with `frame: {sequence, step, held, video}`
  means the image shows exactly that runtime frame. While time is held this is
  the normal result, so an `act` capture shows the step the action reached.
- `uncertain: …` means the shown frame changed during the screenshot, which is
  common in realtime. The image is still an original; its step is unknown.
- `not an Engine stream page` / `no frame shown yet` are what they say.

`capture` sidecars carry the same `frame_correlation` and `frame` fields.

The page screenshot is the **composite**: world plus the product's DOM UI and
HUD. For the world alone, as the runtime drew it, use `world` or `world-frame`:

```sh
playtest assist SESSION --json '{"op":"act","id":"forward","ms":800,"capture":true,"world":true}'
playtest assist SESSION --json '{"op":"world-frame"}'
```

These fetch the runtime's frame route at the page's own frame size (so the
renderer is not resized) and save the payload as sent: JPEG by default, raw RGBA8
when the product runs with `RUSTY_RENDER_STREAM_FORMAT=rgba`. Over MCP, an `act`
or `jump` with `capture`, and `world-frame`, return the image inline with the
result.

## Checking sound

Models here cannot listen to audio. An agent checks sound by recording what
the game played and reading measurements or a spectrogram image. That shows
whether and when sounds played, how loud, and roughly what kind (a noisy
burst or a tone). It cannot judge whether something sounds good.

den-agents has no speakers. Products play to PipeWire's null output
`auto_null`, which records exactly what they played:

```bash
parecord -d auto_null.monitor --file-format=wav sound.wav &   # start before acting
# ... playtest assist actions ...
kill %1
```

Times in the recording count from when it started. Every session and program
on this machine plays into the same output, so a recording also holds other
running games. Check `playtest status` first, or confirm a sound by repeating
its action and seeing it repeat.

- **Anything at all:** `ffmpeg -i sound.wav -af volumedetect -f null -`
  prints mean and peak level; around −90 dB means silence.
- **When things happened,** as loudness per 100 ms (`time:dB`), where a sound
  shows as a jump:

  ```bash
  ffmpeg -nostats -i sound.wav -af "asetnsamples=4410,astats=metadata=1:reset=1,ametadata=print:key=lavfi.astats.Overall.RMS_level" -f null - 2>&1 |
    awk -F'[ =:]+' '/pts_time/{t=$NF} /RMS_level/{printf "%.1f:%.0f ", t, $NF}'
  ```

- **What it was like:** `ffmpeg -i sound.wav -lavfi showspectrumpic=s=1000x300:legend=1 spectrogram.png`,
  then look at the image. Gunshots and impacts are bright vertical bands
  across many frequencies; music and tones are horizontal lines.
- **What the product meant to play:** some products answer audio debug
  commands (Rifles has `rifles.audio.read`; `assist discover` lists them).
  That is the product's intent, not proof the device played it.

Sound from the Windows box is not captured; record on den-agents.

## Local service

The service runs on this machine (`den-agents`, RX 9070 XT) as the user unit
`crew-playtest.service`, listening only on `127.0.0.1:48200`. Products render
on the local GPU in their own runtime; the headless Chromium page only shows
frames and hosts product UI.

The GPU's board power is capped at 250 W (its default is 317 W), because
den-agents shares a UPS with other machines. The cap is set at boot by
`/etc/udev/rules.d/90-den-agents-rx9070xt-power.rules`, and the current value
is in `/sys/class/drm/card0/device/hwmon/hwmon*/power1_cap` (microwatts).
Ordinary playtesting doesn't reach it. If frame rates or GPU timings measured
here seem low for the hardware, check the cap before suspecting the product,
and don't compare them with uncapped numbers from another machine.

- CLI: `~/.local/bin/playtest` (`playtest mcp` for MCP clients)
- Programs: `~/.local/share/crew-playtest/`
- Profiles: `~/.config/crew-playtest/games.json`
- Pool: `~/.config/crew-playtest/pool.json` (`{"size": 10}` here)
- Evidence: `~/.local/state/crew-playtest-local/`

Install or update from this repository with the active sessions stopped:

```sh
PLAYTEST_CHROMIUM=/path/to/chrome scripts/install-playtest.sh
```

The installer builds `playtest`, `playtest-assist` and `playtest-service`,
installs the browser adapter, preserves existing profiles and pool, writes the
unit and restarts it. Without `PLAYTEST_CHROMIUM` it installs Playwright's
Chromium. The `chromium-local` wrapper enables hardware GPU (ANGLE on Vulkan,
WebGPU) for web profiles that use WebGL/WebGPU themselves; Engine products do
not need it, since their page only paints frames. Default headless Chromium
otherwise falls back to SwiftShader; see Chromium's
[headless GPU guidance](https://chromium.googlesource.com/chromium/src/+/HEAD/docs/gpu/using-gpu-hardware-in-headless-chrome.md).

For Codex, `scripts/install-codex-playtester.sh` links the
[product-playtest skill](../codex/skills/product-playtest/SKILL.md) and writes
the `playtester` agent profile; both use this CLI. Claude Code agents use the
same skill, linked into their skills directory. The `windows-box` skill
(`codex/skills/windows-box`) is the short guide to the Windows test box, for
both; link it into `~/.codex/skills` and `~/.claude/skills` the same way:

```bash
for skill in product-playtest windows-box; do ln -sfn "$PWD/codex/skills/$skill" ~/.claude/skills/$skill; done
```

`playtest-service` requires `--games`, `--state`, `--worker`
(scriptworker/worker.mjs) and `--browser-worker` (browser/worker.mjs);
`--chromium` optionally names a browser executable and `--serve-config` a
den-serve configuration for hosted profiles. To use DSH, load
`configs/playtest/dsh.patch.yml` through its normal profile configuration; the
patch launches `playtest mcp` and contains no game logic. The playtest service
does not use or extend the messaging fabric.

## Game profiles and hosts

Use `playtest games` and `playtest game show GAME` to discover profiles and
controls. A profile either names a URL that something else serves, or a
**host** that each session starts for itself:

```json
{"id": "rusty-doom-hosted", "backend": "browser", "environment": "service",
 "host": {"repo": "/home/agent/dev/rusty-doom"},
 "description": "Doom; each session starts its own live-debug host",
 "controls": {"W/A/S/D": "Walk and strafe", "E": "Use"},
 "reset": "Every start and recover creates a fresh host and world; stop ends the host.",
 "interaction_queries": true, "presentation_observations": true}
```

For a hosted profile, `playtest start` reads the repository's ordinary
`.den-serve.json` manifest (optional `host.manifest` names another), starts a
private **instance** of that host through den-serve's broker, waits for its
health check, then opens `host.path` (default `/`) on it. The session owns the
host: `stop` ends it, `recover` starts a fresh one, and a failed start stops it.
The start result's `launch.host` records the port, LAN URL (a human can watch the
tester's world there), launch fingerprint and log paths. Instances appear on the
den-serve status page and in `den-serve list` labelled with their session ID;
they never adopt the checkout's ordinary host or another instance.

Every host of an Engine product on a pair from rusty-engine `0807adf51` or
later takes a per-project lock, so a second concurrent session of that game
needs its manifest to name the instance. Add to its `serve` block:

```json
"instanceArgs": "--instance {instance} --label {label}",
"keepArgs": "--keep",
"stopCommand": "PATH=\"$HOME/.local/bin:$PATH\" rusty dev stop --project ./src/Game/Game.csproj --instance {instance}"
```

`{instance}` is the playtest session ID and `{label}` is
`crew-playtest:<session>`, so `rusty dev list` shows which session owns each
host, and `rusty dev stop` stops the host's whole process tree (runtime and
headless browser too). Add these only together with that pair: an older pair
refuses `--instance` and the host would not start. See
[den-serve instance, keep and stop commands](den-serve.md#instance-keep-and-stop-commands).
To stop a session's host by hand, use
`den-serve stop <project> -repo <repo> -instance <session>` or
`rusty dev stop <id>` from `rusty dev list`; never kill by name pattern.

Hosted starts from one checkout are serialized until each host is ready, because
products such as `rusty dev` build and stage into the checkout. Starting takes
seconds when the product is already built and up to five minutes otherwise; the
CLI waits accordingly. Instances share the checkout's staged build and its
restaging on file changes. For work that edits the checkout while testers run,
point a profile at a separate worktree.

A URL profile (no `host`) shares whatever world that server holds: browser slots
isolate browsers, but every session on one host shares the player, time mode and
game state, and a reconnect is not a reset. Prefer hosted profiles for gameplay
testers. The URL must be reachable from this machine; `localhost` means this
machine. Server reachability, browser launch and product readiness are separate
facts; a loaded page is not proof that a game finished loading or accepts input.

After editing the profiles file, run `playtest reload`; existing sessions keep
the profile they started with.

### Engine backend: no browser

`"backend": "engine"` drives the product host directly over its loopback HTTP
surface; nothing runs Chromium. It answers the same `assist` operations by
posting the product's live-debug commands, and adds:

- **Input** through a harness claim (`control/claim`, Engine #8888). Each
  session claims the runtime's input binding under its slot label
  (`crew-playtest-N`) and posts ordinary input events with increasing
  sequences. An attached page shows "Input held by crew-playtest-N" and sends
  nothing while the claim holds, so window focus and page blur cannot clear
  what the harness holds. The claim lapses five minutes after the last input,
  and `stop` releases it.
- **Captures** from `frames/capture` (Engine #8887): lossless world frames at
  the output's size, named by the simulation step they show, with the drawn
  cameras. A capture never resizes a page or window. Product UI and HUD are not
  in them; attach a browser session to the same host for those.
- **Receipts** keep queued, admitted and observed apart. `delivery: queued`
  is the host's acknowledgement; `admission: admitted by the runtime` is the
  runtime ingesting the events (in held time, with the next advance or debug
  command); the product's effect is whatever the next observation shows. A
  batch whose outcome is unknown is never replayed: the next input takes a
  fresh claim, which clears anything left held.

It exercises binding admission and the product's own mappings, not the page's
input capture: DOM focus, text entry, menus and the pointer-lock shim need the
browser backend. `input` steps work as in the browser lane except `point`
(absolute positions need a page). `capture`, `act` with `capture`, `world-frame`,
`survey` and `record` all write world frames; `record` needs held time, so each
frame is taken at an exact step.

```json
{"id": "rusty-doom-engine", "backend": "engine", "environment": "service",
 "host": {"repo": "/home/agent/dev/rusty-doom",
          "manifest": "/home/agent/dev/rusty-doom/.den-serve-room-study.json"},
 "description": "Doom room study without a browser", "controls": {},
 "reset": "Every start and recover creates a fresh host and world; stop ends the host."}
```

The product must register Engine's `PlaytestDebugModule` for `observe`,
`action` and `act`; Doom's Loading Bay does so only in its room-study build.

`"environment": "windows-desktop"` runs the same backend against a native
Windows build on the Windows playtest box: each session's instance is its own
window there, `assist window` captures it with the product UI, and `assist
os-input` sends real keyboard and mouse input under the box's one foreground
lease. With `"desktop": true` both work on the whole screen instead, for
dialogs and other windows. See [Windows playtest box](playtest-windows.md#two-tiers-of-input),
and [when something is in the way](playtest-windows.md#when-something-is-in-the-way)
for the other ways onto the box.

## Agent entry point

```sh
playtest games
playtest start rusty-doom-hosted
playtest observe SESSION
playtest assist SESSION --json '{"op":"discover"}'
playtest input SESSION --json '[{"kind":"hold","keys":[69],"ms":100}]'
playtest run SESSION --file /absolute/path/trial.js --budget-ms 60000
playtest script SCRIPT
playtest cancel SESSION
playtest stop SESSION
```

`start` launches the session's browser (and host, for a hosted profile) and
focuses the page. The returned session is not a pristine game snapshot: inspect
its screenshot. `connected` means launch completed, not visual acceptance.
Wait for `discover` to report the inspection adapter before issuing actions.

CLI results are JSON. Open returned image paths with your harness's image tool.
`playtest mcp` exposes the same operations and returns image blocks for
`observe`, `capture` and captured `assist` actions. Several clients can inspect
one session; disconnecting a client does not stop it. Each tester gets its own
pool slot. Always stop your session when finished.

## Bounded semantic controller

Use [playtest-assist](playtest-assistant.md) for short Jev-controlled intervals
with a planner-defined goal, finite tactics, observed-fact thresholds and an
interval transcript. It reuses an owned session and the same input service.
This is a parked research track: ordinary adaptive testers do not need it.

## JavaScript API

`run` submits source and returns immediately with a script ID. Poll `script` for
phase, latest checkpoint, and evidence paths. Programs have a 60-second default
wall-clock budget, configurable from 100 ms to 120 seconds, and at most 512 API
calls; engine-backend sessions may make 4096, because an `assist` act in held
time costs milliseconds of wall time rather than the action's duration.
Waiting at a yield counts against that budget. Split longer evaluations
into programs, inspecting observations between them. In held time, `sleep`
does not move the world; advance it with `assist({op: "advance", ms})`.

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
| `controller.hold(state, ms)` | Complete simultaneous Xbox state through the page's virtual gamepad; neutral afterward |
| `keyboard.hold(keys, ms)` | Named key chord; release afterward |
| `input(steps)` | Same validated input batches as CLI/MCP |
| `observe()` | Screenshot metadata/path, with frame correlation; no game-state injection |
| `assist(request)` | Any assist operation, e.g. `assist({op: "act", id: "forward", ms: 300})`; same result as the CLI |
| `interaction(options = {})` | Read optional product interaction facts at the reticle, or a normalized cursor point |
| `checkpoint(label, data)` | Record progress without pausing |
| `yieldToAgent(label, data)` | Pause until `resume SESSION --json VALUE` |
| `sleep(ms)` | Cancellable wait, at most 10 seconds |
| `console.log(...)` | Record values in the program journal |

Sticks `lx/ly/rx/ry` range from -1 to 1; positive X is right, positive Y is up.
Triggers `lt/rt` range from 0 to 1. Xbox names are
`a,b,x,y,lb,rb,ls,rs,start,back,guide,up,down,left,right`.
`keyboard.hold` names include letters, digits, Enter, Escape, Tab, Space, Ctrl,
Shift, Alt, Backspace, arrows, F5, and F6. Raw CLI/MCP `input` batches carry
Windows virtual-key codes: `[{"kind":"hold","keys":[69],"ms":100}]` holds E.
Other kinds are `move`, `point`, `click`, `wait` and `gamepad`. Unknown fields are rejected: use
`buttons: ["back"]`, not `back: true`. Invalid input is rejected before
delivery and does not degrade an otherwise healthy session.

Calls are serialized, including accidentally unawaited calls, and flushed before
the program completes. Use a single controller state for simultaneous movement
and look. Separate holds release between actions; parallel promises do not create
simultaneous holds. Each batch is limited to 10 seconds. These are realtime
inputs; for step-exact play use held time and `assist act`.

## Product interaction queries

`interaction` is an optional, read-only product capability. Profiles opt in with
`interaction_queries: true`; the service discovers the current Engine interaction
catalog and invokes its generated `interaction.query` or `interaction.cursor`
command over the product's live-debug HTTP transport (the session's own host,
for a hosted profile). A query returns service metadata plus the product's facts
unchanged. It never navigates, activates, changes the camera, or issues look
input.

```sh
playtest interaction SESSION
playtest interaction SESSION --json '{"mode":"cursor","x":0.5,"y":0.5,"aspect":1.7777778}'
```

Opt-in alone does not establish availability: each request checks the live
catalog. Responses include `facts`, exact `raw_result`, `query_id`, and
`evidence_path`. Query receipts preserve product numbers; JavaScript consumers
must use the raw JSON with a lossless parser if a product emits identities above
its safe integer range. No identity is ever sent back to activate a target by
this API.

Omit options, or use `{ "mode": "reticle" }`, to query the current reticle. For
`{ "mode": "cursor" }`, provide all of `x`, `y`, and `aspect`: `x` and `y` are
normalized bottom-left coordinates in `[0,1]`, and `aspect` must be greater than
zero. Cursor coordinates are not screen pixels.

Interaction facts are semantic assistance and query evidence, not visual
readiness. Use ordinary input to act, then query again to observe the result.
An unavailable or unknown candidate route remains unknown; callers must not infer
a route or replace it with an input action. When an explicit target-ID action is
required, invoke the Engine CLI's published use command; it reaches the same
product handler and keeps the same freshness, reach, and line-of-sight checks.

## Browser operations and comparable captures

Browser sessions also serve plain web applications. Browser operations use the
same service, CLI/MCP and supervised JS worker:

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

Gamepad input is one private virtual `mapping: "standard"` device through
`navigator.getGamepads()`: a browser API injection, not a hardware emulator.
Axes are normalized to `[-1,1]`, triggers are button values in `[0,1]`, and it
neutralizes after every bounded step. Relative mouse input works while the page
holds pointer lock (`{"kind":"move","dx":40,"dy":-10}`, integers within
-32767..32767; acquire lock with an ordinary click first). These are trusted
Chromium events, not OS input. Long or cancelled browser operations may
terminate the browser process so abandoned input cannot continue; inspect status
and recover explicitly. Idle script completion does not destroy the browser.

Capture sidecars preserve original image paths. `compare_to` references a prior
`capture_id`; results compare known dimensions and supplied viewpoint metadata,
not image quality or game state. Viewpoint and assistance fields are caller
supplied. The only overlay policy is `preserve`; product UI and diagnostics are
not hidden. The final visual reviewer must inspect both original images
directly, record neutral observations before acceptance mapping, and report
pass/fail/uncertainty with image references. Keep runtime diagnostics, assisted
mechanics and ordinary control usability separate.

### Engine presentation observations

For an Engine product, `playtest capture SESSION --json '{"engine_presentation":true}'`
(or `presentation_observations: true` on the profile) also records the
product's `engine.renderer.presentation` answer: the last frame the runtime
drew, its sequence and simulation step, the cameras it drew from (including an
observer override), viewport and view revisions. The sidecar keeps the raw
response, parsed facts and request/response times. Paired captures add
`comparison.engine_presentation`: submitted camera, view layout and viewport
equality, plus runtime/surface agreement. GPU completion and whole-world
readiness remain unavailable. See the upstream
[presentation contract](/home/agent/dev/rusty-engine/docs/presentation-capture.md).

Products own viewpoint visits and any movement they cause. Record the requested
name/pose in `viewpoint`, label that assistance, then compare the frame's drawn
camera and viewport before making a visual judgment.

## Cancellation and recovery

`cancel SESSION` kills a running worker and requests input cleanup; cleanup is
independent of worker cooperation. The session remains for inspection. A script
can finish as completed, failed, cancelled, timed_out, or cleanup_uncertain;
never treat the last state as verified neutralization.

`recover SESSION` stops the old session and starts a new one for the same game,
retaining the previous session ID. For a hosted profile that is a fresh world;
for a URL profile the server's world continues. It never replays a script or
uncertain gameplay input. After a service restart, saved active sessions become
interrupted; their hosts ended with the service, so the reaper's first pass
ends them and frees their slots (otherwise stop or recover them). Programs,
API calls/results, checkpoints, and final state are retained under the service
state directory.

## Expiry and keep

Client disconnection does not stop a session, so the service ends abandoned
ones itself. A reaper checks every slot when the service starts and every 30 s:

- **Idle expiry.** A session that no agent call has named for
  `idle_timeout_minutes` (pool.json, default 30, the Engine's own idle limit;
  0 never expires) is stopped: its browser and host are released and the slot
  freed. Any call naming the session counts (observe, input, run, assist,
  capture, script, …); `playtest status` does not, since inspection never
  renews a session. A session running a script is not expired.
- **Host exit.** When a hosted session's product host exits by itself (Engine
  idle expiry, a crash, `rusty dev stop` or `den-serve stop` from outside), the
  session is stopped too, whatever its phase, rather than held degraded.
- **Keep.** `playtest start GAME --keep` (MCP `start` with `"keep": true`)
  exempts a long live demo from idle expiry, and launches its host with the
  manifest's `keepArgs` so the Engine does not expire it either. `recover`
  keeps the flag. Stop a kept session yourself.

The ended session's `phase` is `stopped` with `end_reason` `idle_expired` or
`host_exited: <reason>`, and `ended_at`. For an Engine host the reason comes
from `rusty dev list --all --json`, which keeps a host that ended for a reason
(`idle-expired`, `port-unavailable`, `project-removed`) for a day; a crash
leaves none, so the reason is `the product host exited`. An agent's own stop
records `stopped`. If cleanup fails, the session stays degraded and the next
pass retries.

The Engine expires a host after 30 minutes without product activity (input, a
lifecycle call, a page attaching); frame pulls do not count. A session that
only observes for that long can therefore end with `host_exited: idle-expired`
before its own idle limit. Use `--keep` for a demo that is watched, not played.

Ended session records older than `history_retention_days` (default 14; 0 keeps
them) are removed from the state directory. Captures, scripts and other
evidence are kept.

## Pool and reload

`playtest start GAME` allocates a free slot and returns `id` and `slot_id`.
When full, start waits in a cancellable FIFO queue (default 15 s, `queue_wait_ms`
in pool.json accepts 0..20000), then returns `pool_busy`. Queued starts are not
persisted or replayed. `playtest status` returns aggregate `pool` occupancy and
a `slots` array; `playtest status SESSION` is specific to that session.
Occupancy is not a GPU-performance guarantee: an unattended game keeps rendering.

`pool.json` also sets `idle_timeout_minutes` (0..10080, default 30) and
`history_retention_days` (0..3650, default 14); see Expiry and keep.

After atomically replacing the profiles file or `pool.json`, run:

```sh
playtest reload
# Equivalent on the same loopback-only API:
curl --fail-with-body -H 'Content-Type: application/json' \
  -d '{"op":"reload"}' http://127.0.0.1:48200/command
```

The response reports profile count, capacity, `queue_wait_ms`,
`idle_timeout_minutes` and `history_retention_days`. A read/JSON/
schema error, duplicate profile ID, invalid size/wait, slot construction failure
or unsafe shrink returns an error and keeps the previous registry, capacity and
queue policy. Unknown fields are rejected. Each profile needs an `id` and either
a `url` or a `host`. A requested profile found on disk but not loaded reports
that `playtest reload` is needed. Existing sessions keep their startup profile.
Growing capacity adds slots and wakes queued starts; shrinking removes only idle
slots at the end. Slot IDs never change. Worker and state paths remain startup
settings.

## Evidence and history

See the [acceptance record](playtest-evidence/README.md) and the
[unprompted evaluator trial](playtest-evidence/tool-choice.md). Earlier evidence
was gathered through a remote Wolf/Moonlight backend on den-srv and a native
controller target; that backend was retired once products rendered on this
machine's GPU. Those records remain as history; their capture and input caveats
describe that transport, not the current service.
