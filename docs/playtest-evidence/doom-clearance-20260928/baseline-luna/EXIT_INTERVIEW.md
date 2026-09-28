# Doom clearance playtest exit interview

## Outcome

- **Result:** incomplete gameplay at the requested 20-minute bound; this was an operationally successful observation run.
- **Profile / session:** `doom-clearance`, `22c7d7ec-b210-4733-9458-0c565662bd58`, slot-1, `http://127.0.0.1:4495/`.
- **Started / stopped:** 2026-09-28 06:45:56Z to 07:05:47Z (19m51s). The browser was stopped and released. The native game host was not stopped or modified.
- **Actions:** 124 action requests recorded (62 physical `act` calls, 24 looks, 6 jump-plans, 4 jumps, 10 clearance queries, 8 time queries, 7 observes, plus discover/targets/action).
- **Final gameplay:** 8/12 kills; health 59; armor 0%; Shotgun; 108 bullets and 22 shells; 9/16 items; alive and grounded at body `(52.65211, 0.17000082, -1.6070111)` on the raised southern walkway. Four enemies remained: 31008, 31009, 31011, 31012. Southern doors 20001/20002/20003 remained closed.
- **Stop reason:** time ceiling, not death or level clear. Further progress remained possible, but I stopped at the bound.

## Neutral visual evidence

The initial scene is a grey/rust loading bay with a central blue toxic pool, green pickup, pistol, and the prompt “Find supplies. Reach the southern terminal. E opens doors.” HUD was health 100, armor 0, pistol, 50 bullets, 0 kills, 0/16 items. The initial original screenshot was opened and retained at [initial-23d54dc3.png](/tmp/doom-clearance-luna/initial-23d54dc3.png).

The east-area original screenshot shows brown platforms separated by bright green toxic pools, a live imp ahead, and a dead body on the walkway: [east-current-23e8c93b.png](/tmp/doom-clearance-luna/east-current-23e8c93b.png). The southern blocker screenshot shows the toxic pool and a raised walkway ahead: [southern-wall-blocker-b71dd494.png](/tmp/doom-clearance-luna/southern-wall-blocker-b71dd494.png). After an ordinary jump, the camera was on the raised walkway beside the toxic pool with an imp firing from an upper ledge: [southern-raised-walkway-d0265232.png](/tmp/doom-clearance-luna/southern-raised-walkway-d0265232.png). The final scene shows the raised walkway, medikit and shells ahead, and toxic pools on both sides: [final-15e9ab7a.png](/tmp/doom-clearance-luna/final-15e9ab7a.png).

## Route and combat

1. Started in the loading bay, killed west trooper 31002 with ordinary aim and pistol fire.
2. Recovered around west walls, collected the Shotgun, shells, and stimpack; health returned to 100.
3. Approached north door 20000, aligned ordinary look, pressed `E`, and verified the door raised to 2.5 and opened. The interaction receipts show the approach changing from out-of-reach to Ready/selected, then the normal use transition.
4. Crossed the north wing and killed troopers 31001, 31003, and 31004; collected the shotgun, medikit, and shells along this route.
5. Entered the east hall, killed imp 31005, collected east bullets 30010, and killed imp 31006.
6. Used an ordinary jump toward the prior stuck target near `(52,-0.75,-16.5)`, landed grounded, then used two distinct nearer ordinary jump targets to climb back onto the raised southern walkway.
7. Killed upper imp 31010 and southern trooper 31007. Stopped on the raised walkway before reaching the southern doors.

The north-door approach was grounded at `(7.004207,0.9199989,-30.959805)`, yaw `71.500015`, pitch `-9.1`, with `aimHit` entity 20000 at 1.17m. A normal look then put the player at yaw `90.000015`, pitch `0`, with `aimHit` entity 20000 at 1.10m and interaction `Ready`, selected id 20000 revision 2. Ordinary `E` advanced 200ms and moved to `(7.128694,0.9199989,-31.001549)` while the door was moving; moving forward next reached the door wall at `(7.8300004,0.9199989,-31.001549)` with wall contact entity 20000. The player then crossed after the door opened. These receipts are `act-north-door-reach-1.json`, `look-north-door-aim.json`, `use-north-door-executed.json`, and `act-through-door-clearance-setup.json`.

## Clearance and jump findings

All clearance and time receipts are retained under this directory. `time` before and after the clearance batch both returned `advancedMs: 0`, `simulationStep: 5281`, and `worldHeld: true`; clearance is read-only in this run.

- **Same-foot query:** `/tmp/doom-clearance-luna/clearance-initial-same-feet.json` returned `targetStatus: no-overlap` and no contact translation.
- **Short initial query:** `/tmp/doom-clearance-luna/clearance-initial-short.json` returned no target overlap but `translationStatus: contact-at-start`, `sweep.converged:false`, and a floor contact. This pattern also appeared on later floor-start queries.
- **North doorway:** after normal `E`, the open-door target query returned no target overlap but a start-floor contact. A threshold target overlapping entity 20000 was recorded in `/tmp/doom-clearance-luna/clearance-door-threshold.json`. This is a raw telemetry result, not proof that the open door was impassable: the live 4495 baseline includes disabled/trigger dynamic colliders in capsule queries, so dynamic-collider false positives remain a known harness/runtime limitation.
- **Old stuck target:** from the raised east area, `/tmp/doom-clearance-luna/clearance-south-stuck-target.json` returned `targetOverlap.present:false`, `supportBelowTarget.converged:true`, and a slope-suitable support normal `[-0.1170, 0.9775, 0.1755]`. The straight sweep still reported current-floor `contact-at-start` and `converged:false`. The normal is useful evidence of a possible support surface, not a path or landing guarantee.
- **Real southern blocker:** forward motion from `(51.743137,-0.5999995,-14.594024)` advanced about 1m and stopped against a static wall with normal `[0.5547,0,-0.8321]`; health fell to 90 while under fire. `/tmp/doom-clearance-luna/clearance-real-south-raised-target.json` reported no target overlap for `(52,-0.75,-16.5)` and a converged, slope-suitable support probe, while the sweep stayed at the current lower floor. A nearby raised target `(52,-0.75,-14)` overlapped static instance 10062. These facts explain the vertical platform transition only partially; they do not promise a route.
- **Recovery attempt 1:** `jump-plan` and `jump` toward `(52,-0.75,-16.5)` advanced 800ms and landed grounded at `(52.446655,-0.5999995,-15.3033695)`. The old stuck-airborne state was not reproduced.
- **Recovery attempt 2:** after a second lower-floor wall stop at `(56.71792,-0.59892744,-11.258863)`, a jump toward `(55.5,-0.75,-9.5)` landed grounded on the raised walkway at `(55.248375,0.17000087,-9.136665)`.
- **Recovery attempt 3:** after another lower-floor stop at `(51.787235,-0.59998894,-4.391682)`, a jump toward `(54,-0.75,-2)` landed grounded on the raised walkway at `(52.65211,0.17000082,-1.6070111)`. This made the next southern trooper reachable and enabled kill 8.

## Feature classification

| Feature | Result | Evidence / limitation |
|---|---|---|
| `clearance` operation available | Pass | Returned body, overlap, sweep, support, translation, and slope fields on every query. |
| Clearance does not advance simulation | Pass | Before/after `time` receipts report `advancedMs:0` and unchanged step 5281. |
| Target overlap and support facts | Pass as telemetry; uncertain as traversal proof | Static floor/platform contacts are explicit, but several sweeps are nonconverged and current-floor `contact-at-start`; support caution says it is not a stand/path guarantee. |
| Open-door interpretation | Uncertain | Raw threshold overlap included entity 20000; live baseline may include disabled/trigger dynamic colliders and create false obstruction. Ordinary `E` opened and the run crossed north door successfully. |
| Old airborne snag reproduction | Not reproduced | Ordinary jump landed grounded every time; the exact airborne pose was not observed. |
| Southern walkway recovery | Pass for this run | Two distinct jump targets reached the raised walkway and enabled continued combat. |
| Full gameplay clear | Incomplete | 8/12 kills at the time ceiling; player remained alive. |

## Friction and suggestions

### Gameplay

1. The toxic room gives only a short prompt (“Find the raised walkway”) while the lower floor and raised walkway look similar; a clearer visual/height cue would reduce trial jumps.
2. Falling or stepping to the lower floor repeatedly forced a jump back to a nearby raised target. The ordinary jump worked once a target was chosen, but the route was difficult to infer from the camera.
3. Enemy hitscan pressure during wall and platform recovery reduced health from 100 to 59 before the time bound.

### Harness/runtime

1. Clearance reports a useful support probe, but the straight capsule sweep commonly reports current-floor `contact-at-start` with `converged:false`, even when a normal floor support exists. A separate explicit “start surface vs. target surface” interpretation would make the result easier to act on.
2. Baseline capsule queries include disabled/trigger dynamic colliders. Keep those raw facts visible, but filter or classify them before presenting them as movement obstructions.
3. `jump-plan` was useful as a bounded ordinary-input hint, but it correctly refused targets beyond its window. The report should keep the actual landing pose/grounded state as authoritative.

### Missing facts

1. Clearance does not expose a reliable route or jump landing guarantee; it should remain explicitly read-only and diagnostic.
2. The query did not make the raised platform’s exact usable boundary obvious. A small visual or semantic surface label would help distinguish lower floor, raised walkway, and toxic hazard.
3. A capture paired with the exact collision instance and normal at the current wall would make the southern blocker easier to interpret without relying on inferred geometry.

## Receipts

- Session metadata: `/tmp/doom-clearance-luna/start.json`
- Initial capture metadata: `/tmp/doom-clearance-luna/capture-initial.json`
- Final observation: `/tmp/doom-clearance-luna/final-observe.json`
- Final capture metadata: `/tmp/doom-clearance-luna/final-capture.json`
- Action/observation event journal: `/tmp/doom-clearance-luna/events-final.jsonl`
- Cleanup: `/tmp/doom-clearance-luna/stop.json`
- Complete raw query/action receipts: `/tmp/doom-clearance-luna/`
