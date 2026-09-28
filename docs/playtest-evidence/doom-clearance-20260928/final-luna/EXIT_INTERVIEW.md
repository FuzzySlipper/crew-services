# Luna Doom clearance playtest — final exit interview

## Run and endpoint

- **Profile:** `doom-clearance-final`
- **Session:** `63702cae-8aa0-49a3-8efb-fc0f4ecaff39`, slot-2
- **Native host:** `http://127.0.0.1:4496/` (left running for the parent)
- **Backend:** local crew-services browser, action-driven world
- **Primary mission result:** `uncertain` at the requested 20-minute ceiling. The run did not clear the level and did not end in death; it ended alive while still navigating/combat-testing.
- **Operational result:** pass. Ordinary controls, diagnostics, original captures, and browser cleanup worked.
- **Ceiling/stop timing:** gameplay was stopped when the parent reported the 20-minute bound; the final read-only capture completed at `2026-09-28T07:15:42.115Z` during the wrap sequence. No gameplay actions were issued after the stop instruction.

## Final state

The final read-only state was captured before stopping:

- Position/pose: center approximately `(49.94838, -0.57999945, 6.3545084)`, yaw `-89.16709`, grounded.
- Health `36%`, armor `76%`, shotgun, 5 shells and 70 bullets.
- Kills `6`; five live hostiles remained in the final catalog: `31012`, `31008`, `31011`, `31009`, and `31001`.
- Door `20000` (north wing) was open. Southern study door `20001` remained closed; terminal room and exit remained available.
- Final screenshot shows the player against a metal wall above the green toxic channel, red damage overlay, and the banner “Toxic waste! Find the raised walkway.”

## Compact route and action chronology

1. Captured the neutral loading-bay scene, discovered the assist surface, and set time to `action-driven`.
2. Moved north through the loading bay, collected the shotgun, shells, medikit, armor, and north supplies. Killed troopers `31003` and `31004`.
3. Opened north door `20000` from a physically usable side and crossed it. A clearance query against the opened door target was run from within eight units; before and after time receipts both reported simulation step `5211` and `advancedMs: 0`.
4. Reached the raised north/east platform, collected shells, and traversed toward the toxic east hall.
5. At the prior snag region, captured a before image and semantic pose. The ordinary jump to a nearby raised-walkway target landed grounded near `(51.73804, 0.17000087, -18.34666)`, distance `0.30` from its target. No frozen airborne state occurred.
6. Used additional ordinary jump hops to cross from toxic floor to the raised walkway, including a landing near `(54.276608, 0.17000087, -17.067026)`. The after image visibly shows the brown walkway over toxic green channels with live imps ahead.
7. Killed east imps `31005`, `31006`, and `31010`; killed southern trooper `31007`. Combat receipts show distinct `enemy-projectile` and `enemy-hitscan` damage as well as `hazard` damage.
8. Continued toward the southern rooms using ordinary movement and incremental jump targets. The ceiling arrived with the player alive at the final toxic-channel pose above.

## Per-feature verdicts

### North-door clearance and crossing — pass

`clearance` was read-only and reported:

- Target feet `(8.25, 0, -32)` from player center `(15.169422, 0.91549015, -31.631292)`.
- `currentOverlap.present: false` and `targetOverlap.present: false`.
- Upward support below the target was present.
- The straight sweep reported `contact-at-start` / `startSolid` against static floor geometry.
- The player then crossed the opened door with ordinary forward movement.

The query did not mutate time: `north-door-clearance-time-before.json` and `north-door-clearance-time-after.json` both report step `5211`, action-driven mode, and `advancedMs: 0`.

### Clearance interpretation — useful but ambiguous

Raw start-floor contact makes the result harder to interpret. A sweep contact at the starting capsule/floor is not by itself proof that the target is an impassable wall: the north-door target had no overlap and was physically crossed afterward. Reports should separate `currentOverlap`, `targetOverlap`, support facts, and initial sweep contact instead of collapsing them into a single blocked verdict.

At the original snag target `(52, -0.75, -16.5)`, the query reported no current overlap, no target overlap, valid support within the slope limit, and a start-floor sweep contact. The exact target was outside the bounded jump window, but a nearer target was available and the ordinary jump landed grounded near the walkway. The observed movement resolved the prior snag without teleporting.

### Physical jump around the raised walkway — pass for the exercised path

Every executed `jump` response returned `grounded: true` after settling, with input released. No frozen airborne state occurred. The old freeze pose was not reproduced. Longer direct targets can correctly return `target-beyond-bounded-jump-window`; incremental targets were needed to cross the level geometry.

### Combat and level clearance — incomplete

Combat input and semantic targeting worked, but the time ceiling stopped the run at 6 kills with 5 live hostiles. The southern terminal and exit were not entered, so full-level clearance is unverified.

## Damage observations

Damage source was identifiable per observed hit in product telemetry during this run:

- `source: enemy-hitscan`, message `Under fire!`
- `source: enemy-projectile`, message `Imp fireball!`
- `source: hazard`, message `Toxic waste! Find the raised walkway.`

Health fell while walking on toxic floor and stopped while successfully standing on raised walkway segments, supporting the hazard attribution. The final state still carried the latest hazard receipt even after later enemy hits, so the latest damage record should not be treated as a complete cumulative breakdown.

## Debug 422 provenance

Several browser captures retained a console error at:

`http://127.0.0.1:4496/__rusty/product/runtime/debug/execute`

with HTTP `422 (Unprocessable Content)`. The page had no page errors, ordinary movement/combat continued, and the session remained usable. The closest known trigger was an unsupported read-only `assist` map request during bounded spatial inspection; the CLI returned `unknown inspection operation`. The browser console does not include the request body, so this is a likely provenance rather than an exact request-to-console correlation. It should be treated as a rejected debug/inspection request, not as a gameplay or infrastructure failure.

## Evidence index

All paths below are under `/tmp/doom-clearance-final-luna/` unless an absolute path is shown.

- `initial-neutral.png`, `initial-neutral.json`
- `initial-observe.json`, `initial-targets.json`, `initial-interaction.json`
- `route-north-initial.json`, `grid-initial.json`, `probe-initial.json`
- `act-open-north-door.json`, `act-through-north-door-2.json`
- `north-door-clearance-before.json`
- `north-door-clearance-time-before.json`, `north-door-clearance-time-after.json`
- `before-original-snag.png`, `before-original-snag.json`, `before-original-snag-observe.json`
- `original-snag-clearance-before.json`, `original-snag-jump-plan.json`
- `jump-original-snag-near.json`, `after-original-snag-jump.png`, `after-original-snag-jump.json`
- `clearance-east-wall.json`, `jump-plan-east-wall.json`, `jump-east-wall.json`
- `jump-snag-short-hop.json`, `jump-back-walkway-near.json`, `jump-south-walkway-gap.json`
- `act-kill-east-imp-31005.json`, `act-kill-east-imp-31006.json`, `act-kill-imp31010-toxic-close.json`
- `act-kill-south-trooper-31007-ground.json`
- `final-neutral.png`, `final-neutral.json`, `final-observe.json`, `final-targets.json`, `final-time.json`
- `stop-receipt.json`, `post-stop-status.json`

The original final capture remains at:

`/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/63702cae-8aa0-49a3-8efb-fc0f4ecaff39/becc33f5-6ed4-4b13-9fea-dcab19e93e43.png`

## Ranked friction and improvement suggestions

### Game

1. Make the raised-walkway route continuous enough that ordinary movement does not repeatedly drop the player into toxic channels between short platforms.
2. Keep hostile targets on the same reachable elevation as the visible walkway or provide a clear combat vantage point; several shots struck static walkway geometry while enemies were below.
3. Make southern door/terminal access visibly readable from the successful walkway landings.

### Tools

1. Explain `contact-at-start` and `startSolid` as separate sweep facts, with explicit current-versus-target overlap and a concise interpretation hint.
2. Let `jump-plan` expose a bounded reason and a suggested intermediate target when a direct target is beyond the jump window.
3. Preserve target/pose/collision facts in the same receipt as jump results so a landing can be assessed without a second query.

### Telemetry

1. Correlate debug HTTP status, operation name, and request body with browser console entries; the 422 endpoint alone is insufficient for exact provenance.
2. Keep damage source, message, and simulation step together with movement/floor height so hazard and enemy damage can be separated over time.
3. Include the collision entity/instance and contact normal in a human-readable movement summary alongside raw fields.

## Cleanup

Owned browser cleanup succeeded:

- `stop-receipt.json`: `browser_closed: true`, `released: true`, `slot_id: slot-2`.
- `post-stop-status.json`: session phase `stopped`.
- Native host 4496 was intentionally left running for the parent.
