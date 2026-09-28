# Doom follow-up visible verification — exit interview

## Scope and session

- **Profile / configured identity:** `doom-followup`
- **Configured product:** Doom E1M1, `http://127.0.0.1:4494/`
- **Playtest service:** local `crew-playtest.service`, `http://127.0.0.1:48200`
- **Owned browser session:** `583aede9-f688-41af-85f2-1819cdd0eb91`, slot-1
- **Session created/stopped:** `2026-09-28T05:28:00.081Z` / `2026-09-28T05:45:32.808Z` (UTC)
- **Actions:** 121 action results, below the 200-action ceiling
- **Runtime metadata:** profile reported `interaction_queries:true`, browser backend, title `Loading Bay — Doom E1M1`, one local canvas at 1280x720, `engine_queries:false`; the browser rendered and accepted ordinary keyboard input. No model identity is applicable to this game session.
- **Cleanup:** `playtest stop` returned `browser_closed:true`, `released:true`, `slot_id:"slot-1"`. The native host was left running.
- **Scope discipline:** no product source/configuration was read or edited, no cheats/teleport/direct state mutation, and no alternate harness was used.

## Neutral visible start

The initial screenshot showed a first-person industrial loading bay: grey metal floor, rust/orange wall panels and columns, a dark ceiling, a blue toxic pool in the center, and a green pickup beside the pool. The pistol was equipped. HUD showed `BULLETS 50`, `SHELLS 0`, `ARMOR 0%`, `HEALTH 100%`, `KILLS 0/12`, `ITEMS 0/16`; the prompt read “Find supplies. Reach the southern terminal. E opens doors.”

Evidence: [initial-d8fc647b.png](initial-d8fc647b.png), [initial-observe-receipt.json](initial-observe-receipt.json), [events-final.jsonl](events-final.jsonl).

## Exit state and mission result

**Result: incomplete / uncertain gameplay outcome.** The run ended with the player alive at a southern toxic-pool traversal blocker. It was not a death and it did not clear the level.

- **Kills:** 7/12. Killed troopers/imps `31002`, `31001`, `31003`, `31004`, `31005`, `31006`, `31010`.
- **Remaining enemies at the final observation:** `31007` (trooper, 30), `31012` (imp, 60), `31011` (trooper, 30), `31008` (imp, 60), `31009` (trooper, 30).
- **Player:** health 72, armor 0%, Shotgun equipped, 108 bullets, 23 shells, HUD items 9/16, grounded false during the final attempted jump at `(52.844994, -0.088025, -15.674429)`.
- **Last damage:** hazard, “Toxic waste! Find the raised walkway.” The earlier east-wing projectile receipt recorded 8 health lost from “Imp fireball!”.
- **Doors:** north wing door `20000` open; southern study `20001`, terminal study `20002`, and lower passage east `20003` remained closed.
- **Rooms/route:** loading bay -> west corridor -> north approach -> north wing -> east toxic-pool edge. The southern study/terminal were not reached.
- **Stop reason:** after distinct wall, strafe, retreat, jump, and walkway recovery attempts, the visible route remained unresolved; session was stopped at 05:45:32Z within the requested 20-minute/200-action bound. Progress still appears possible because the player was alive and a raised walkway was visible, but this run did not establish the route.

Final visual evidence: [final-toxic-pool.png](final-toxic-pool.png), [events-final.jsonl](events-final.jsonl). The final screenshot shows a rust-metal room, a raised brown walkway at the player’s edge, a large green toxic pool ahead/right, and a corpse/pickup on the walkway; HUD shows 7/12 kills and 72% health.

## North-door interaction proof

Normal movement reached the north door after several collision recoveries. The useful in-range receipt records:

- pose `(5.907955, 0.9164, -30.441847)`
- yaw `82.3°`, pitch `0°`
- candidate door `20000`, distance `2.307 m`, visible
- focus `Ready`, selected `20000`
- `yawDeltaDegrees:+7.7`, `pitchDeltaDegrees:0`
- `aimHit` and focus data were used only as signed read-only guidance; the door was opened with ordinary `E`.

`use-north-door.json` records the door beginning to raise (`raised:0.45`, state opening). A normal forward action then crossed the doorway, and later observations reported `raised:2.5`, state `open`; two enemies inside were subsequently killed.

Earlier blocked approach receipts show the concrete friction:

- `act-north-route-1.json`: `(-20.83886, 0.67, -12.327269)`, yaw `59.4°`, pitch `0°`, `blockFlags:"Wall"` with static contact; a right strafe recovered the route.
- `act-north-route-3.json`: `(-8.061649, 0.92, -19.730003)`, yaw `60.3°`, pitch `0°`, wall contact with +Z normal; a right strafe recovered it.
- `recovery-right-north-door-2.json`: `(2.4128, 0.9, -24.79085)`, yaw `25.2°`, pitch `-6.6°`, blocked at a corner; a left recovery opened the approach.

Evidence: [interaction-north-door-in-range.txt](interaction-north-door-in-range.txt), [use-north-door.json](use-north-door.json), [observe-north-door-after-use.json](observe-north-door-after-use.json), [act-through-north-door.json](act-through-north-door.json), [milestone-north-door-reach.png](milestone-north-door-reach.png), [milestone-inside-north-wing.png](milestone-inside-north-wing.png).

## Southern blocker and diagnostic evidence

At the east toxic pool, an ordinary right strafe from the raised edge entered the lower toxic floor. The receipt recorded health 87, position `(50.113686, -0.5799995, -15.274572)`, and `lastDamage.source:"hazard"`. A left recovery reached `(48.29484, -0.569299, -18.7168)` with `blockFlags:"Wall"`, contact normal approximately `[0, 0.929, 0.370]`, `floorProbePresent:false`, and `stepAttempted:true`. The screen showed the wall immediately to the left and green toxic pool to the right.

At that real blocker:

- `time-before-south-blocker.json` and `time-after-south-blocker.json` both report `advancedMs:0`, simulation step `5272`.
- `grid-south-blocker.json` returned a 9x9x9 grid at cell size 0.25. It showed static collision bands at several layers and a local diagonal/edge pattern, but its own meaning says free cells are not walkability or body-clearance proof.
- `probe-south-blocker.json` hit the nearby static mesh at 0.27–0.48 m on headings 0/45/90 and returned floor hits, while some head/other headings were empty. It is ray sampling, not a capsule route test.
- The receipts therefore show that grid/probe are operational, read-only, and useful for local collision context, but they did not identify a complete walkable route around the toxic pool.

Evidence: [milestone-toxic-blocker.png](milestone-toxic-blocker.png), [grid-south-blocker.json](grid-south-blocker.json), [probe-south-blocker.json](probe-south-blocker.json), [time-before-south-blocker.json](time-before-south-blocker.json), [time-after-south-blocker.json](time-after-south-blocker.json), [recover-backward.json](recover-backward.json), [act-south-walkway-1.json](act-south-walkway-1.json).

## Jump checks

1. **Reachable short ground jump: pass.** `jump-plan-short-ground.json` returned an available ordinary Space+W plan toward feet target `(-7, 0, 2)`. The physical jump landed at `(-7, 0.919999, 1.792034)`, within 0.209 m of the target, with `grounded:true` and no health/kills change. This was ordinary input; it did not teleport.
2. **Toxic-floor recovery: pass with direct observed effect.** From the wall/pool corner, `jump-plan-toxin-floor.json` produced a bounded ordinary jump plan toward `(48.5, -1.5, -21)`. The physical jump landed at `(49.307354, 0.170001, -19.869291)`, `grounded:true`, with no additional health loss. This visibly returned the player to the raised edge. Evidence: [jump-toxin-floor.json](jump-toxin-floor.json), [milestone-jump-recovery.png](milestone-jump-recovery.png).
3. **Second raised-walkway estimate: uncertain/failed to settle.** A plan toward `(52, -0.75, -16.5)` was available, but the physical jump ended at `(52.844994, -0.088025, -15.674429)` with `grounded:false`, `SteepSlope` contact, and downward velocity. Repeated read-only `advance` calls did not change that pose; a short forward action returned `no-observed-movement`. This confirms the plan is only a bounded estimate and actual landing/collision must be inspected.

The jump feature is therefore useful for a short physical recovery, but it does not guarantee that a target feet coordinate is a valid landing surface.

## Feature verdicts

| Feature | Verdict | Evidence / limits |
|---|---|---|
| `discover` | **Pass** | `discover.json` listed the ordinary assist operations. |
| Action-driven time | **Pass** | `time-action-driven.json`; setup and blocker checks showed no time advance for read-only queries. |
| `interaction` CLI | **Pass** | `interaction-cli-default.txt` and `interaction-cli-reticle.txt`; signed yaw/pitch deltas, focus, aimHit, and door state were available. |
| `grid` | **Pass operationally; limited diagnostically** | `grid-default.json` and `grid-south-blocker.json`; no simulation advance, but no walkability/capsule guarantee. |
| `probe` | **Pass operationally; limited diagnostically** | `probe-distance-2.json` and `probe-south-blocker.json`; sampled rays only. |
| `jump-plan` | **Pass as bounded estimate** | Returned both available and refusal reasons; it did not predict the later steep-slope landing. |
| `jump` | **Pass for short recovery; uncertain for raised target** | Two actual ordinary jumps inspected; one returned to a walkway, one remained airborne/steep-slope. |
| Observe movement / `lastDamage` | **Pass** | Receipts exposed position, grounded state, wall/steep-slope contacts, hazard and projectile damage. |
| Normal look + ordinary `E` use | **Pass** | North door was focused and opened with physical keyboard `E`; forward action crossed it. |
| Southern traversal | **Incomplete / uncertain** | Visible toxic pool and walkway were reached, but the southern doors were not reached or opened. |
| Level completion / all kills | **Incomplete** | 7/12 kills, alive at stop; no death or clear-level state. |
| Session cleanup | **Pass** | Stop receipt confirms browser closed and slot released; native host retained. |

## Tools and assistance used

- `playtest` CLI against the local service: `start`, `status`, `assist`, `interaction`, `input` was not needed for gameplay, `capture` artifacts, and `stop`.
- Ordinary visible controls: physical keyboard WASD, normal relative look through assist look (the same product look rules), ControlLeft fire, E use, and Space+W through the bounded ordinary jump assist.
- Read-only semantic assistance: `discover`, `time`, `observe`, `targets`, `interaction`, `grid`, `probe`, and `jump-plan`. Interaction, grid, probe, and jump-plan influenced route/aim decisions and are labeled as diagnostic assistance; they are not unaided visual proof.
- `grid`/`probe` were intentionally re-run at the real southern blocker to compare their static data with the visible movement contact.
- Original browser screenshots and the complete event journal were retained under this directory.

## Ranked friction and suggestions

### Gameplay

1. **Raised-walkway route is visually and physically ambiguous.** The prompt says to find the raised walkway, but entering the visually adjacent area can drop the player onto toxic floor and cost 5 health. Add a clearly readable landing/route cue or a safe recovery edge near the first pool.
2. **Collision corners require repeated manual recovery.** North-wall contacts were eventually solved by strafing; the southern pool corner needed back/forward attempts and a jump. A player-facing collision response or stronger visual affordance would reduce trial-and-error.
3. **Combat pacing exposes weapon cooldown.** A second attack immediately after a shotgun blast was refused as `weapon-not-ready`; waiting while moving solved it, but the refusal was a gameplay friction episode.

### Harness / diagnostics

1. **Interaction facts were valuable once enabled.** Signed yaw/pitch, focus, aimHit, and the north-door selected state directly explained the successful ordinary `E` use.
2. **Grid/probe need route-oriented interpretation.** Their output accurately described local static geometry but cannot answer the player-sized clearance or walkable-route question. A clearly labeled capsule sweep or “reachable walkway candidates” query would be more useful, if later permitted.
3. **Route-call syntax was easy to misapply.** Initial route attempts used the wrong field and were rejected; the documented `id` field is the working form. This was a CLI/API friction point, not evidence that the route was impossible.
4. Browser status included a few `ReadPixels` performance warnings and 422 debug-execute console errors, but live rendering, input, and assists continued; these did not explain the southern blocker.

### Missing facts

1. The interaction response at distant positions can say `route:Unknown`/occluded without providing a player-sized route or reasoned next approach.
2. The grid does not encode walkability, slope usability, or the player capsule radius in its answer; the probe does not solve that gap.
3. Jump-plan’s target-foot coordinate frame and terrain acceptance are not enough to predict a landing on a steep edge; actual pose/grounded inspection remains mandatory.
4. The visible scene does not expose a reliable semantic marker for which side of the toxic pool contains the next raised walkway or which door to try first.

## What worked and next tactic

The strongest sequence was: acquire shotgun/shells and healing, use interaction only to correct ordinary aim/focus, open the north door with normal E, cross it with normal movement, then use observed enemy aimHit to make bounded shotgun attacks. Grid/probe were safe to call because they did not advance simulation. A short physical jump successfully recovered the player from the first toxic-floor drop.

If this world were continued, the next tactic would be to use the visible raised platform as the anchor: remain on its top surface, make short lateral jumps toward the next clearly elevated tile while checking `grounded` after each, then approach lower-passage door `20003` at `(51.25, 0.375, 10)` before the study/terminal doors. Collect the visible stimpack/shells/medikit on the southern side, kill `31007`, `31012`, `31011`, `31008`, and `31009`, and then test `20001`/`20002` with the same focus-then-E sequence. Progress remains possible, but this run stopped before proving it.

## Receipt index

- Setup/initial: `initial-d8fc647b.png`, `initial-observe-receipt.json`, `discover.json`, `time-action-driven.json`, `observe-after-setup.json`.
- Diagnostic setup: `interaction-cli-default.txt`, `interaction-cli-reticle.txt`, `grid-default.json`, `probe-distance-2.json`, `time-before-grid.json`, `time-after-grid-probe.json`, `jump-plan-short-ground.json`, `jump-short-ground-result.json`.
- North route/door: `act-north-route-1.json`, `recovery-right-around-wall.json`, `act-north-route-3.json`, `recovery-right-north-door-2.json`, `interaction-north-door-in-range.txt`, `use-north-door.json`, `observe-north-door-after-use.json`, `act-through-north-door.json`.
- Southern blocker/recovery: `evade-imp-31010.json`, `recover-toxic-left.json`, `grid-south-blocker.json`, `probe-south-blocker.json`, `jump-plan-toxin-floor.json`, `jump-toxin-floor.json`, `act-south-walkway-1.json`, `jump-plan-raised-walkway-near.json`, `jump-raised-walkway-near.json`, `observe-after-raised-jump-1s.json`, `act-airborne-toward-platform.json`.
- Visuals: `milestone-north-door-reach.png`, `milestone-inside-north-wing.png`, `milestone-toxic-blocker.png`, `milestone-jump-recovery.png`, `final-toxic-pool.png`.
- Lifecycle: `status-pre-stop.txt`, `stop-receipt.txt`, `events-final-pre-stop.jsonl`, `events-final.jsonl`.
