---
name: product-playtest
description: Test games and web applications through crew-services playtest using sessions that own their product host, Engine assist actions with held time, browser keyboard/pointer/gamepad input and DOM operations, supervised JavaScript, and step-correlated original capture evidence. Use for visible product evaluation, interaction testing, reproduction, and visual comparisons without editing the product.
---

# Product Playtest

Use the installed crew-services `playtest` CLI or its matching MCP tools.
The maintained capability and setup reference is
[the service guide](/home/agent/dev/crew-services/docs/playtest.md); read the relevant
sections when choosing controls or scripting operations. This skill uses
crew-services throughout. Do not fall back to the retired Den browser broker,
Python controller, or an independently launched browser when a session fails.

## Parent and observer roles

For independent product observation, the coding/reviewing parent spawns
`agent_type: "playtester"` with a neutral mission. The worker operates and
observes; the parent owns implementation changes and acceptance mapping.
A worker already in that role does not spawn another playtester.

Supply the product/repository identity, profile or owned session ID, service URL
when nondefault, ordinary controls, requested observations, and any useful
scenario guidance. State whether an existing session should be retained or
stopped. A URL alone is not a configured profile: the parent owns adding a
profile or arranging service setup when discovery finds none.

The worker may read this skill and the service guide, use the playtest CLI/MCP,
write temporary JS test programs, inspect returned JSON evidence, and open
original images with its image tool. It must not inspect/edit product source,
repair services, change profiles, deploy replacements, or construct another
harness. Missing MCP registration is not a blocker when the CLI works.

## Local workstation service

On `den-agents`, Engine products render in their own runtime on the local
RX 9070 XT and stream frames to a headless Chromium page, which also hosts the
product's UI. The API binds only to `127.0.0.1:48200`; no remote machine is
involved. Local product servers use `http://127.0.0.1:PORT/`; do not copy
historical `192.168.1.22` profile URLs.

Profiles are in `/home/agent/.config/crew-playtest/games.json`, pool size in
`/home/agent/.config/crew-playtest/pool.json`, and evidence under
`/home/agent/.local/state/crew-playtest-local`. `local-gpu-check` is an
infrastructure check, not a game acceptance test. The parent adds product
profiles or changes pool size, then runs `playtest reload`. Reload preserves
existing sessions; invalid files and shrinks that would remove occupied slots
are refused. Do not restart the service to register a product.
See [local service](/home/agent/dev/crew-services/docs/playtest.md#local-service).

This backend supplies browser keyboard/pointer actions and virtual gamepad
input, including relative `move` while the main document holds pointer lock.
Acquire lock with an ordinary click first. Use integer `dx`/`dy` in
-32767..32767; unlocking rejects subsequent relative movement. These are
trusted Chromium events, not OS mouse injection.

## Discover the execution environment

```sh
playtest games
playtest game show PROFILE
playtest status
```

The CLI is `/home/agent/.local/bin/playtest`. The default API is
`http://127.0.0.1:48200`, owned by `crew-playtest.service`. Use `PLAYTEST_URL` or
`playtest --url URL ...` for a supplied alternative loopback service.
`playtest mcp` exposes the same service operations; discover their current
schemas rather than assuming a tool-name prefix or a fixed tool count.

- `backend: "browser"` runs headless Chromium on the service machine. Use its
  DOM inspection/actions and pointer/keyboard capabilities, including relative
  movement under pointer lock. When `capabilities.gamepad` is true, gamepad
  steps inject a standard browser Gamepad API device; this is virtual
  controller input, not native hardware evidence.
- `backend: "engine"` has no browser. It drives the product host through
  live-debug, a labelled input claim and runtime frame captures, so it does
  not depend on window focus. Its captures are world frames named by their
  simulation step, without product UI or HUD; its input tests the product's
  bindings, not the page's input capture (DOM focus, menus, text entry,
  pointer lock). Use a browser profile for those.
- `environment: "windows-desktop"` runs that backend against a native Windows
  build on the Windows box (profiles `rusty-rifles-windows`,
  `rusty-doom-windows`): `assist window`
  captures the window with its UI, and `assist os-input` sends real
  keyboard/mouse input under the box's single foreground lease (click first
  for pointer lock; `foreground_busy` means wait). Add `"desktop": true` to
  either to see or click the whole screen (dialogs, other windows); when
  that is not enough, the runbook lists further ways in; the `windows-box`
  skill covers the box outside playtest sessions. Never RDP into the box. A lane's first start builds
  the product and can take minutes. The box, its agent and building Engine
  there are in
  [the Windows runbook](/home/agent/dev/crew-services/docs/playtest-windows.md).
- Sound: you cannot listen, but you can check it. Record the null output
  (`parecord -d auto_null.monitor`) around your actions, then read loudness
  over time or a spectrogram image with ffmpeg
  ([checking sound](/home/agent/dev/crew-services/docs/playtest.md#checking-sound)).
  Say what was measured, not how it sounds.
- Read returned capabilities and report unsupported operations explicitly.
  Do not substitute one input type and call the evidence equivalent.

A **hosted** profile (`"host": {"repo": ...}`) starts a private product host for
each session: your world is your own, `stop` ends it and `recover` gives a
fresh one. `launch.host` in the start result records its port, LAN URL and
logs. A **URL** profile opens a server someone else runs; every session on it
shares one world, and a new browser is not a reset. `playtest game show
PROFILE` states which applies. The worker never starts or stops a product
server itself; the service does that for hosted profiles.
The service allocates an independent slot from its configured pool. `playtest
status` lists `pool` occupancy and per-slot `slots`; `status SESSION` inspects
your session. Retain both returned session ID and slot ID. Never stop or recover
another agent's session. If all slots are occupied, start queues briefly and may
return `pool_busy`; report that limitation or retry later without taking over a
slot. Other demos may keep rendering while their agents are idle; capture
`pool_activity` records occupancy, not a guarantee of isolated performance.

## Run the session

1. Use the supplied owned session, or `playtest start PROFILE`. Retain the
   returned session ID. If start fails, inspect the returned error and
   `playtest status`; return the observed limitation instead of provisioning a
   workaround. A degraded/interrupted owned session can be stopped or recovered.
2. Capture with `playtest observe SESSION` or `playtest capture SESSION` and
   open the returned original image. Record the visible initial scene before
   deciding whether it satisfies the mission. `connected` establishes launch,
   not asset readiness, focus, game input consumption, or visual acceptance.
   A hosted start may take up to a few minutes while the product builds.
3. Alternate bounded actions and observations. Use JS for a useful sequence,
   loop or conditional; direct commands remain appropriate for short probes.
   Verify the downstream effect of an interaction, not only its first reaction.
   Use before/after images for movement, camera changes and state transitions.
4. On uncertainty, inspect bounded diagnostics and distinguish them from what
   was visible. Treat scenario hints as fallible: retain contradictions rather
   than adjusting the observation to fit an expected answer.
5. Stop owned sessions with `playtest stop SESSION`, including after a failed
   mission, unless explicitly asked to retain them. Check the cleanup receipt;
   use `status` for discrepancies. Client disconnection does not stop a session.

## Engine products: assist actions and held time

For an Engine product, prefer the adaptive assist surface over raw input. It
reads live product facts and pairs ordinary input with simulation time:

```sh
playtest assist SESSION --json '{"op":"discover"}'
playtest assist SESSION --json '{"op":"time","mode":"action-driven"}'
playtest assist SESSION --json '{"op":"observe"}'
playtest assist SESSION --json '{"op":"act","id":"forward","ms":400,"capture":true}'
playtest assist SESSION --json '{"op":"look","yaw":30}'
playtest assist SESSION --json '{"op":"world-frame"}'
```

Wait until `discover` reports the inspection adapter before acting. In held
time the world only moves when you act or `advance`, so you can think between
steps. `act` resolves the product's live binding and duration, presses the
ordinary key or button, advances, releases and returns the observation and
delta; with `capture` its image is returned inline and names the frame and
simulation step it shows. Use `survey`, `record`, spatial queries and the
observer camera as described in the
[service guide](/home/agent/dev/crew-services/docs/playtest.md#adaptive-engine-playtesting).
An accepted action is not a confirmed hit, door or pickup: read the result.

## Inputs and browser operations

Use profile controls. Raw batches have explicit kinds and millisecond holds:

```sh
playtest input SESSION --json '[{"kind":"gamepad","lx":0.3,"rt":0.5,"ms":400}]'
playtest input SESSION --json '[{"kind":"hold","keys":[87],"ms":200}]'
playtest input SESSION --json '[{"kind":"move","dx":35,"dy":-15}]'
playtest input SESSION --json '[{"kind":"point","x":640,"y":460,"width":1280,"height":720},{"kind":"click","button":1,"ms":100}]'
```

Raw keyboard holds use Windows virtual-key integers; the JS helper accepts
named keys. Controller sticks range -1..1, positive X right and positive Y up;
triggers range 0..1. Buttons use `buttons: ["a"]`, not `a: true`.
Holds release afterward. Batches are bounded to 10 seconds. These are real-time
inputs, not admitted simulation-update counts or deterministic replay; in held
time use `assist act`, or advance explicitly after input. A delivered movement
does not establish pointer lock or game consumption: use ordinary
click/Escape/refocus controls and observations when testing acquisition/loss.

For a browser-capable session:

```sh
playtest browser SESSION --json '{"op":"inspect"}'
playtest browser SESSION --json '{"op":"fill","selector":".new-todo","value":"Example"}'
playtest browser SESSION --json '{"op":"press","selector":".new-todo","key":"Enter"}'
playtest browser SESSION --json '{"op":"click","selector":"button[type=submit]"}'
playtest browser SESSION --json '{"op":"near","x":300,"y":200,"max_distance":80}'
playtest browser SESSION --json '{"op":"select","token":"RETURNED_TOKEN","action":"click"}'
```

DOM assistance is bounded near-cursor selection, not game-world targeting.
`select` defaults to moving only; activation requires explicit `action: "click"`.
Tokens are short-lived and single-use. Respect stale, obstructed, disabled,
ambiguous and no-candidate outcomes. Use a specific locator after a strict-mode
ambiguity rather than assuming the first matching element is intended.
Record when DOM/semantic inspection or assistance influenced the result;
assisted interaction evidence does not prove unaided visual discovery or aiming.

## World objects: avoid repeated pixel hunting

For a world container, door or talk target, inspect the product's Engine debug
catalog before spending a long sequence guessing screen coordinates. Products
using `InteractionDebugModule` expose `interaction.help` and `interaction.inspect`.
The latter returns current labels, IDs/revisions, reach/visibility/availability,
rejection reasons and exact `useCommand` values. Use the matching runtime's
`rusty-live-debug --origin URL --command "interaction.inspect"`.

When target-ID assistance fits the requested test, execute the reported
`interaction.use <id> <revision>` through that same CLI. It is an explicit
mutating assisted action using the product's ordinary use handler; it removes
reticle precision only. Approach normally when out of reach, resolve occlusion,
and reinspect stale identities. Verify the actual resulting UI with the normal
playtest browser tools. Record this as assisted interaction, not proof of a
physical click. If the mission tests picking itself, use ordinary pointer input.

Missing commands are a product integration gap, not a reason to invent browser
gameplay hooks. The parent can adopt the shared Engine `WorldInteraction` and
`InteractionDebugModule`; see `/home/agent/dev/rusty-engine/docs/controller-interaction.md`.
The existing `playtest interaction` query below remains read-only.

## Optional product interaction queries

For a profile with `interaction_queries: true`, use `playtest interaction SESSION`
or `await interaction()` in a script. The service checks the product's generated
Engine catalog; missing support reports `capability_unavailable`. Read returned
`facts` and retain `query_id`/`evidence_path`. Query facts are semantic assistance,
not screenshot freshness or permission to activate an old target observation.
Approach, cycle and use through ordinary controller/keyboard input, then query
again. Preserve product availability, visibility and unknown route outcomes.

Free-cursor queries use `interaction({mode:"cursor",x:0.5,y:0.5,aspect:width/height})`.
Supply known viewport-local normalized bottom-left coordinates and viewport aspect;
do not substitute mouse-look deltas. Queries never turn, navigate or activate.
Label captures and reports when these facts guided the test.

## Jev-assisted control intervals (research)

This is a parked research track; ordinary missions do not need it. Use it only
when the parent supplies an interval configuration. For repeated
navigation/combat decisions with useful text observations, it runs
`playtest-assist` on the existing owned session. Read the
[assistant guide](/home/agent/dev/crew-services/docs/playtest-assistant.md) for setup,
spatial-map interpretation and the complete gamepad + concurrent-parent example.
The supervising agent supplies the goal, finite tactics and stop conditions;
Jev selects actions while an optional parent model periodically revises guidance.
Use this as a bounded debugging tool, with visual inspection before and afterward.

Combine compact product facts with a small `spatial.map ascii` read when supported.
Read its axes, legend, Y intervals and revisions: collision blanks do not prove
walkability, navigation may be unknown, and maps are omniscient assistance.
Use fresh target/weapon/interaction facts for immediate actions. Jev receives text only. The harness parent is text-only by default;
`parent_vision: true` attaches the current PNG pixels to its request. Use
`capture_every: 1` for a current image on every parent request and ensure capture
paths are readable where `playtest-assist` runs. Check transcript `input_image`
provenance; screenshot paths alone do not give either model vision.
New product commands require explicit harness allowlist support, not merely a
catalog entry. The guide explains the supported observation commands.

The parent arranges configuration, profile and model routes; an observer can run
an already supplied interval without editing the product or repairing services.
Do not drive the session manually or with a second script while it runs. Preserve
the JSONL transcript, inspect the actual handback reason and input cleanup, then
capture/inspect the result. A reached kill/objective threshold ends the interval
before its budget expires; continuing requires a revised goal/threshold. The
runner leaves the session open, so stop it when the mission is finished unless
asked to retain it. Record semantic/gamepad aim assistance in the final report.

## Compose supervised JavaScript

Write a temporary `.js` file and submit it with:

```sh
playtest run SESSION --file /absolute/path/trial.js --budget-ms 60000
playtest script SCRIPT_ID
playtest resume SESSION --json '{"keys":["W"]}'
```

`run` returns immediately. Poll `script` for completion, failure or a yielded
checkpoint; submitting a program is not evidence that it finished. Examples
below use different backend-specific capabilities; select those the session
supports.

```js
// Game session with a virtual gamepad: combine movement/look in one state.
await controller.hold({ly: 0.5, rx: 0.25}, 300);
checkpoint('After movement', await observe());
const choice = await yieldToAgent('Choose the next action', await observe());
await keyboard.hold(choice.keys, 200);
```

```js
// Browser session: compose UI actions and preserve original comparison images.
await browser({op: 'fill', selector: '.new-todo', value: 'Example'});
await browser({op: 'press', selector: '.new-todo', key: 'Enter'});
const before = await capture({label: 'created'});
await browser({op: 'click', selector: '.toggle'});
checkpoint('Compare originals', await capture({label: 'completed', compare_to: before.capture_id}));
```

Other APIs are `input(steps)`, `sleep(ms)` and journaled `console.log(...)`.
Calls serialize; parallel promises do not create simultaneous controller/key
holds. The default program budget is 60 seconds, configurable from 100 ms to
120 seconds, with at most 512 API calls. Yield pauses count against the budget.
Split longer evaluations into programs and inspect results between them.

`cancel SESSION` terminates a running script and requests input cleanup. An
idle browser stays alive; cancelling an active browser operation can terminate
that browser and require explicit recovery. `recover SESSION` stops the old
session and returns a new session ID for the same profile. Re-inspect it before
continuing: progress may be lost, and product state may persist elsewhere.
Never automatically replay an action with unknown delivery. Report
`cleanup_uncertain` as uncertainty, not verified neutralization.

## Evidence and judgment

`observe` and `capture` return original image paths and metadata; the MCP can
return images inline. `capture` accepts `label`, `compare_to` (a previous
capture ID), caller-supplied `viewpoint` and `assistance`, and overlay policy
`preserve`. Do not hide product UI or diagnostics. Comparison metadata describes
known geometry and supplied viewpoint agreement; it is not a visual verdict.

For Engine products, `capture({engine_presentation:true})` (or the same CLI/MCP
capture option) records separate submitted camera/viewport/publication facts.
Profiles may enable this with `presentation_observations:true`. Inspect
`engine_presentation.facts` and `comparison.engine_presentation`, including
pending state, observation age and runtime/surface identity. A pending snapshot
can still show an older submitted camera. Requested viewpoint metadata is not
an observed pose; product viewpoint visits are explicit assisted movement.
It does not establish GPU completion, whole-world readiness or acceleration.
Missing support is recorded while the original capture remains useful.

Each screenshot also reports which runtime frame it shows:
`frame_correlation: "frame-sequence"` with `frame: {sequence, step, held}`
when the page showed the same frame before and after the screenshot (the
normal case in held time), otherwise `uncertain`. The composite image includes
the product UI; `world-frame` is the runtime's world image alone.

Inspect originals directly. The final visual judge must see the original
images, record neutral observations, then map acceptance and state uncertainty.
Keep screenshot evidence, runtime diagnostics and ordinary-control usability
separate. Supplied viewpoint/assistance is not independently observed state;
screenshot dimensions do not establish canvas backing resolution, GPU rendering
or GPU rendering; frame identity comes only from the reported frame fields.
Use actual available metadata and leave missing facts unknown. Engine readiness and world-target integration must be advertised by
the current service before use; do not invent a debug endpoint or game state.

Retain returned session/script/capture IDs, absolute image/sidecar paths and
journal paths. Cleanup success and evidence completeness are separate: report
persistence errors while preserving original artifacts. Do not reconstruct a
canonical history after a partial journal write.

Return a compact report:

- Mission and profile/session, with the execution backend and whether the
  world was the session's own host or shared.
- Primary outcome: `pass` (visible mission succeeded), `fail` (observed product
  failure), `uncertain`, or `infrastructure_error` (environment prevented testing).
- Neutral initial scene, important changes and unexpected details.
- Actions/reproduction steps; scripts, assistance or diagnostics used.
- Direct original-image links and relevant capture/script/journal paths.
- Cleanup receipt and any evidence or input-release uncertainty.

Useful control/navigation difficulties or replacement scenario notes can be
included for the parent. The worker does not publish shared guidance or create
follow-up engineering work. A successful neutral-observation mission need not
imply that the product passed a separate visual acceptance criterion.
