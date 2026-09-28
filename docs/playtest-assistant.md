# Bounded playtesting assistant

`playtest-assist` runs a short controller interval on an existing caller-owned
playtest session. Jev chooses a tactic; the ordinary playtest service executes
its fixed keyboard, mouse or controller batch. This is a debugging assistant,
not a general agent harness or a replacement for visual playtesting.

Use the installed `playtest-assist`, or build from the service repository with
`go build -o /tmp/playtest-assist ./cmd/playtest-assist` and invoke that absolute path.
Start and visually inspect a session using `playtest`, then supply its ID in a
configuration based on `configs/playtest/assistant.example.json`:

```sh
playtest-assist --config /tmp/interval.json --output /tmp/interval.jsonl
playtest-assist --config /tmp/interval.json --output /tmp/baseline.jsonl --baseline
```

The session must be exclusively owned for the interval. The runner leaves it
open for planner inspection and requests input cleanup on every exit. Stop it
with `playtest stop SESSION` when finished. SIGINT/SIGTERM cancels the interval;
finite input batches still bound holds if the client is killed abruptly. An
uncertain input receipt is never retried automatically. Inspect the handback's
cleanup receipt; a cleanup error is not proof inputs were released.

## Start a useful interval

Use this for short navigation, combat or interaction debugging when repeated
control decisions benefit from fresh text facts. The supervising agent owns the
mission, initial visual inspection and final judgment. Jev chooses from the
supplied finite tactics; an optional parent model updates strategic guidance.
Jev receives text only. The parent can optionally receive the current screenshot
with `parent_vision: true`; see the visual-parent configuration below.

1. Discover the profile with `playtest games` / `playtest game show PROFILE`,
   start an owned session, and inspect its original screenshot and capabilities.
   Focus/reset through the product's ordinary controls before the interval.
2. Copy [the gamepad + parent example](../configs/playtest/doom-gamepad-parent.json)
   for current Doom, or [the generic example](../configs/playtest/assistant.example.json)
   for a smaller mission. Replace `session_id` and set `product_url` to the
   profile's exact URL (including its path/trailing slash). Gamepad tactics need
   a session advertising gamepad support and product controller bindings.
3. Inspect the live debug catalog and sample the selected observations. Use the
   map guidance below to choose a useful area/resolution; verify progress and
   stop pointers against actual returned facts. Set a bounded goal, budget and
   health/ammunition or other product-specific handback conditions.
4. Run the copied configuration with a new transcript path:

   ```sh
   playtest-assist --url http://127.0.0.1:48200 \
     --config /tmp/doom-interval.json --output /tmp/doom-interval-01.jsonl
   ```

   The service URL is the playtest API, distinct from the product URL. Override
   it when using another supplied service. Do not run a second controller or
   manual inputs against the same session during the interval.
5. Inspect the returned reason, final facts, original screenshot and cleanup
   receipt. A threshold stop, model handback, timeout and death mean different
   things. The session remains open: the supervisor may inspect/reframe and run
   another bounded interval, or stop it. Use cumulative thresholds appropriate
   to the current state: another unchanged `kills >= 2` interval immediately
   stops once two kills already exist. Update the goal text as well.

Remove `parent_model`, `parent_protocol` and `policy.parent` for fixed-policy
Jev. `--baseline` instead repeats the first tactic without a model; it is not
Jev without a parent. Keep model aliases deployment-specific.

## Spatial maps and compact gameplay facts

Combine a small spatial map for route context with compact product facts for
immediate decisions. Doom's working example uses:

```json
"commands": ["combat.observe", "spatial.map ascii 15 2"]
```

`spatial.map <ascii|json> <radius> <cellSize>` uses radius in **cells**, from
0 through 15, and positive world-unit cell size. Radius 15 gives 31x31 cells;
at 2m this covers 62m per side. A smaller `spatial.map ascii 6 1` gives 13x13
cells for nearby door/corner work. Prefer ASCII for model route context; JSON
is useful for structured inspection and exact cell fields. Both represent the
same bounded spatial read. Large JSON maps plus previous observations increase
model input substantially; begin small and widen only when the route requires it.

Read the returned legend and metadata rather than guessing the glyphs:

- Columns advance +X and rows +Z; this is world-aligned, not screen-aligned.
  Use player position/facing and the product's control signs to choose turns.
- Collision `#` means a cell footprint intersects retained/supplied collision
  in the stated Y interval. Blank means no hit in those sources, **not proven
  walkability**, loaded empty space or sufficient character clearance.
- Navigation is a separate layer: `.` allowed, `x` disallowed, `?` unknown.
  Check `navigationPresent`, support heights and revisions. A game's enemy
  pathfinding grid does not by itself establish that this debug projection is
  published. Preserve unknown when projection/samples are missing.
- Entity overlays can hide a collision glyph. Read the separate collision
  layer and legend's stable IDs, labels, states and world positions. Dead actors
  may remain annotated; the glyph alone does not identify a living target.
- The map is omniscient semantic assistance, not the agent's visual field.
  Check collision/navigation Y intervals, out-of-view counts and truncation;
  this X/Z view is not a complete stacked-floor representation.

Use maps to suggest openings, approaches and door routes; use fresh product
facts for moving targets, readiness and firing. Doom `combat.observe` supplies
health/ammo, live enemy IDs, bearings/pitch errors, LOS, doors and current weapon
ray previews. Enemy-center LOS is not a hit guarantee. With gamepad assistance
active, inspect `player.aimAssist.shot.assistedHit`; a StaticMesh obstruction
still calls for repositioning. Assistance does not shoot through geometry.

The supervisor can inspect `spatial.map-at` through the product's normal
`rusty-live-debug` CLI when the catalog exposes it, to center a read on a door
and floor support height without moving the player. The assistant's current
allowlist accepts `spatial.map`, **not** `spatial.map-at`. It also accepts
`combat.observe`, `loading-bay.readout`, `interaction.help` and
`interaction.inspect`; arbitrary catalog commands are not automatically allowed.
Use inspect/use guidance below for fiddly containers rather than repeated clicks.

For another game, publish semantic annotations and a compact read-only snapshot
at its existing debug boundary; Engine owns `Spatial.ReadMap` and
`SpatialMapSnapshot`, while the product owns health, hostility, objectives and
control meaning. A new observation command also needs an explicit assistant
allowlist integration. Do not add browser gameplay scraping or a second spatial
implementation. See [Engine spatial maps](/home/dev/rusty-engine/docs/csharp-sdk.md#spatial-debugging-maps)
and [Doom command details](/home/dev/rusty-doom/docs/spatial-inspection.md).

## Observations and policy

The observer preserves original screenshot metadata and optional read-only
product observations. Jev is text-only: it cannot see the screenshot merely
because its path is present. `commands` may select the Engine spatial JSON map
or Doom's product readout. The URL must exactly match the session profile;
commands are checked against the live product catalog. These are semantic
assistance, not screenshot-derived facts. Spatial inspection is optional.
An empty command list permits capture-only observations, but a meaningful
controller needs suitable textual facts; do not imply visual perception.

Planner configuration supplies a goal, instructions, tactic descriptions and
fixed input steps. Each tactic is at most two seconds; total interval is at
most120 seconds. Jev cannot synthesize scripts, commands, durations or inputs.
`max_actions: 0` removes the action-count cap; `stall_actions: 0` disables
automatic stall handback. The wall-clock budget remains mandatory. Use these
for time-limited endurance trials and retain explicit observed stop conditions.
The deterministic baseline repeats the first tactic using the same observation,
threshold, deadline and cleanup machinery.

`progress_pointers` are JSON pointers into the observation (including `facts`).
Unchanged selected facts for `stall_actions` consecutive actions cause handback.
Missing selected facts also cause handback. Select facts that measure useful
progress, such as player X/Z, rather than clocks, revisions or unrelated actors.
`stop_when` conditions use `eq`, `lte`, or `gte` and are evaluated independently;
any matching condition returns its supplied reason. Numeric comparisons require
numeric facts. Product-specific thresholds belong in this caller configuration.

The JSONL transcript records policy, timestamped observations, controller
inferences and raw probability responses, requested actions, input receipts,
latency and final handback. A model's `goal_complete` remains
`controller_goal_complete`; only an explicit observed threshold receives the
planner's threshold reason. Schema validity and confidence do not establish
correctness. Review original screenshots and product facts when interpreting it.

## Model route

Jev uses a typed decisions request, not chat completions. The default router is
`http://127.0.0.1:18082`, alias `jev`, with local `POST /v1/decisions` forwarding
to OpenRouter `POST /api/alpha/decisions`. `--router` and `--model` override these.
`PLAYTEST_ROUTER_TOKEN` supplies optional router authentication; the OpenRouter
provider key remains in den-router. No provider credentials enter transcripts.
The choice question includes permitted tactics and four handback choices:
completion, stalled, unexpected state, and uncertainty. Low confidence,
malformed responses and request timeouts return control without input.

## First live Doom interval (task8375)

Using the existing CoreCLR Doom session, both controllers approached the nearby
bullets with two 350 ms forward holds, then handed back when the observed Z
coordinate crossed -1.5. Original screenshots showed “Picked up Bullets”,
70 bullets, and ITEMS1/16. The model did not receive image pixels.

| Controller | Actions | Interval | Decision calls | Reported model cost |
| --- | ---: | ---: | --- | ---: |
| Fixed-forward baseline | 2 | 1.289s | none | none |
| Jev 1.13 through den-router/OpenRouter | 2 | 1.726s | 158 ms, 222 ms | $0.00071169 |

The two Jev choices were `forward`, with confidence 0.87 and 0.79. Full textual
observations consumed 5827 and11118 input tokens because the second decision
also included the previous snapshot. Keep maps small; larger intervals may
benefit from selecting a more compact observation. These are two calls, not a
latency benchmark or evidence of general gameplay competence. A fixed script
is already sufficient for this simple approach. The experiment establishes
ordinary-input continuation, real Jev decisions, bounded handback and useful
inspection evidence; branching or difficult navigation remains future usage.

Local original evidence and interval transcripts:
`/home/dev/evidence/task8375/`. Each transcript includes the full policy, captured
facts, original image paths, response probabilities and cleanup receipt. Final
browser/Engine warning capture is report-only without a compatible baseline.
The report retained two WebGL ReadPixels performance warnings and one aborted
`audio-feedback` request on the separate warning-capture browser attachment.
Both interactive intervals had no page errors. This attachment diagnostic is
not explained by the assistant changes and is not presented as a clean warning
delta; no rendering or audio correctness claim is made by this experiment.

## Concurrent parent feedback

A configuration may add `parent_model`, `parent_protocol` (`responses` or
`chat`), and `policy.parent`:

```json
{
  "parent_model": "codex-gpt-5.6-luna",
  "parent_protocol": "responses",
  "capture_every": 5,
  "policy": {
    "parent": {
      "every_actions": 10,
      "timeout_ms": 15000,
      "max_age_ms": 15000,
      "event_pointers": ["/facts/combat.observe/player/kills"]
    }
  }
}
```

This is a fragment to merge into a complete interval configuration. Without a
parent configuration, the fixed-policy Jev mode is unchanged. The parent starts
from the first observation and is asked again after the configured number of
actions, or when a selected event fact changes. There is at most one parent
request in flight. Jev continues on its current guidance while that request
runs. Completed guidance is installed at an action boundary; stale, invalid or
failed results are recorded without replacing current guidance.

Parent output describes the situation, objective, target, parameters and
preferred tactic IDs. These are semantic guidance for Jev, not generated input
scripts. The parent cannot change the caller's tactic menu, deadlines or stop
thresholds. Its `stop` request causes a labeled handback. The transcript records
guidance revisions, source observation time, parent latency, and how many
ordinary actions completed while the parent was thinking. The last 15 action
IDs are supplied as recent context.

`combat.observe` is Doom's compact read-only gameplay observation. It can be
combined with `spatial.map ascii` for a smaller local map than the JSON cell
array. Both are product/Engine observations, not browser-inferred game state.
`capture_every` reduces screenshot frequency; skipped captures are explicit,
not copies falsely labeled fresh. Screenshots remain original local evidence;
the parent receives pixels only when explicitly enabled. Keep the browser rendering.

`cycle_timing` separates observation work, Jev request latency, input-call
duration and gaps between input calls. `observation_age_at_input_ms` measures
wall time since that observation began; it does not establish the age of a
rendered frame or exact input-consumption tick. Controls remain finite batches:
concurrent parent updates do not imply continuous held input during Jev calls.

Parent routes are deployment-specific. On this machine `codex-gpt-5.6-luna` and
`codex-gpt-5.6-sol` use the Responses streaming endpoint; `glm-5.3` is a chat
route. Check availability before drawing model-quality conclusions. Parent
completion/failure and latency belong in the experiment report alongside kills,
damage, ammunition and route progress.

A complete starting configuration is
[`doom-parent-feedback.json`](../configs/playtest/doom-parent-feedback.json).
For gamepad aim assistance use [doom-gamepad-parent.json](../configs/playtest/doom-gamepad-parent.json).
See [the experiment record](playtest-parent-feedback-experiment.md) for the
successful follow-up and historical fixed-policy/Luna/Sol/GLM comparison.

For world-object interaction, observations may include `interaction.inspect`
and `interaction.help` from the Engine's shared `InteractionDebugModule`.
They list target identities, labels and current use/rejection facts. Explicit
`interaction.use <id> <revision>` is a mutating assisted action for the supervising
agent through `rusty-live-debug`, not an observation command or a hidden Jev
control. It invokes the ordinary product handler after fresh reach/visibility
checks. See the Engine's `docs/controller-interaction.md` green path before
resorting to repeated pixel hunting for containers or doors.

## Optional visual parent

Set `parent_vision: true` alongside `parent_model` and `parent_protocol` to attach
the current observation screenshot to the parent as a native image content part.
Both `responses` and `chat` transports support this. Jev still receives only its
text decision view, including the parent's resulting maneuver guidance.

Use `capture_every: 1` when every parent request needs a current frame. Skipped
captures are explicitly unavailable; older images are not silently substituted.
The image path must be readable on the machine running `playtest-assist` (a shared
filesystem is necessary for a remote capture service). An unreadable requested
image fails that parent request instead of pretending visual input was delivered.
The transcript records image provenance without embedding the image payload.

The [visual Doom example](../configs/playtest/doom-navigation-visual-parent.json)
uses Luna, GPS guidance, and a current capture for each observation.

Give the parent a stable local maneuver, completion condition and recovery rule,
such as “turn toward the fresh waypoint until aligned, then advance; back away
and reassess if displacement stays near zero.” The image and facts precede the
model response: Jev must use current bearings, not repeat an old angle. Screenshots
provide visible layout; spatial maps/routes provide explicit semantic assistance.
Neither establishes pixels and queried game facts came from the exact same tick.

## Compact decisions and simultaneous controls

The optional top-level `decision_view` selects named JSON pointers for Jev only.
The parent retains the full original facts/map; the transcript also retains the
original observations. `fields` copies selected JSON values, `deltas` reports
current minus previous numeric values, and `positions` reports x/y/z plus
measured dx/dy/dz/displacement for position objects. Missing facts or missing
prior samples are explicitly `unknown`, not zero. For example:

```json
"decision_view": {
  "fields": {"player": "/facts/combat.observe/player", "enemies": "/facts/combat.observe/enemies"},
  "deltas": {"health_change": "/facts/combat.observe/player/health"},
  "positions": {"movement": "/facts/combat.observe/player/position"}
}
```

Without that setting, the existing full semantic context is unchanged. A local
map can be useful to the parent while current target facts and measured changes
make a smaller tactical input. A zero displacement does not by itself prove
collision; interpret it alongside the preceding requested movement.

`policy.control_groups` optionally partitions fixed tactic IDs into movement,
look, trigger or other groups. Each group has `id`, `instructions`, and `tactics`
(the allowed tactic IDs). All referenced tactics must be single gamepad steps
with the same hold duration, and groups must own disjoint control fields.
Neutral options may omit axes. The runner asks one typed choice per group plus
one continue/handback choice in a single hosted Jev request, then merges the
selected fixed fields into one ordinary finite input. Jev cannot invent axis
values, buttons or durations. An invalid or missing choice does not dispatch.
Confidence gating uses the minimum returned confidence across required choices;
per-question option distributions and the original response remain recorded,
without claiming that confidence is calibrated or overriding the selected option.

`policy.decision_max_age_ms` discards a tactical reply if its source observation
is older than the configured wall-time limit. Zero leaves this check disabled.
A stale reply is journaled as `decision_discarded`; the runner reobserves within
the original interval deadline. This differs from a network timeout and does not
retry an uncertain input. Observations begin before optional screenshot capture,
so capture time contributes to this conservative age. Avoid starting a known
hold that cannot finish within the interval: the runner reserves 100ms transport
headroom and waits out the remaining budget without sending that input. Unknown
transport delays can still produce an uncertain receipt; inspect cleanup.

[The composed Doom example](../configs/playtest/doom-composed-parent.json) uses
these options. It is experimental: `minimum_confidence: 0` retains low-confidence
choices for gameplay measurement, while explicit handback/invalid-response/death
and deadline checks remain. A 0.1 minimum across four answers caused early look
ambiguity handbacks in initial trials. Neither setting is calibrated for Doom;
record the setting and compare matched policies. Its `--scripted-doom` comparison mode requires removing parent
configuration and uses a deliberately small Doom-specific heuristic instead of
Jev: turn toward a living LOS target, fire only on a matching weapon ray, and
recover after repeated blocked forward attempts. This is an explicit scripted
reference, not Engine navigation, and its timing has no model round trip.
It requires the example's movement/look/trigger IDs. Existing `--baseline`
continues to mean repeat-first-tactic and requires single-choice mode.

### Optional route guidance

A product can expose `navigation.targets` (named destinations) and
`navigation.route <id>` (fresh, read-only Engine route facts) through its existing
live-debug catalog. To enable the GPS aid, add an explicit initial destination:

```json
"navigation": { "initial_target": "REPLACE_WITH_REPORTED_TARGET_ID" }
```

Use an ID from `navigation.targets`. The parent can change destinations with its
`navigation_target` field. This is separate from the free-text combat `target`.
An empty navigation target retains the previous destination; unknown IDs do not
replace it. Every observation refreshes the selected route. The observation
stores it under the stable `navigation.route` fact key, suitable for a compact
view field such as `"route": "/facts/navigation.route"`. Keep the destination
catalog in the parent context; Jev usually needs only the current route.

The aid never presses controls, follows a path automatically, opens doors, or
teleports the player. Jev must turn toward the next waypoint, move, avoid immediate
obstacles, use doors normally, and fight. Treat unavailable, blocked, exhausted,
or unreachable results as such; a route proposal does not establish physical
arrival or certify traversability. A new parent destination takes effect with a
new observation so its guidance is not paired with the old destination's route.

For comparisons, give both variants the same mission and destination catalog;
omit `navigation` in the unaided variant. Keep the model, finite control menu,
budget, stop conditions and initial state matched. Record destination arrivals,
exploration and lack of movement as well as kills and survival. Spatial maps and
routes are explicit omniscient assistance, not evidence of unaided perception.

The [Doom navigation example](../configs/playtest/doom-navigation-parent.json) starts with the north-wing door. Doom currently reports static routes and live door hints separately; some physically traversable small steps may be absent from its conservative grid (Engine follow-up #8399).

The first three-per-mode Sol/Jev GPS comparison (60 seconds each) did not improve
navigation: aided kills averaged 0.67 versus 1.0 unaided, with 31.3 versus 36.0
explored 2 m bins; all survived and neither mode opened the objective door. Treat
this as an available route-observation tool, not a demonstrated autonomous
navigator. Short cell-center waypoints, weak turn-following and conservative
route coverage remain practical limitations. Evidence: Den Engine task #8398,
`/home/dev/evidence/jev-navigation/README.md`.

The Luna visual-parent follow-up (task8400) verified actual image delivery. Three
gameplay trials with vision produced0/0/1kills and one60s survivor; three matched
text-only Luna trials produced0/0/0kills and no survivors. Median parent latency
was7.13s visual versus6.39s text. Three separate Jev-timeout attempts were retained
as infrastructure failures and replaced once each. No trial reached the door.
This small sample does not establish visual superiority; repeated distant-door
use choices remain a controller-guidance issue. Full evidence:
`/home/dev/evidence/jev-luna-vision/README.md`.
