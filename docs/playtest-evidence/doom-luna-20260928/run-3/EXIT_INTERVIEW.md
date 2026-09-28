# Luna Doom playtest — run 3 exit interview

## Outcome

- **Operational result:** pass. The browser session, native host, ordinary controls, semantic observation, captures, and cleanup all worked.
- **Mission result:** blocked / uncertain product acceptance. The run ended with the player alive at a genuine navigation/interaction blocker; it did not reach victory or death.
- **Session:** `2c045a19-4ddd-43b0-80d7-2a2bf4137b61` (`doom-luna-3`, native host port 4493).
- **Final state:** 8/12 kills, 8/16 items, health 9%, armor 75%, 24 shells and 88 bullets. Four enemies remained: IDs `31008`, `31012`, `31009`, and `31011`.

## Compact route and action chronology

1. **Initial loading bay:** Captured the neutral starting scene. The blue central pit and green pickup were visible; HUD showed pistol, 50 bullets, 100 health, 0 armor, 0/12 kills, and 0/16 items. Evidence: `initial-neutral-42c87d45.png`, `initial-neutral-42c87d45.json`.
2. **West supply route:** Collected the shotgun, shells, armor, and bullets. Killed west troopers `31002` and `31001`.
3. **North wing:** Killed troopers `31003` and `31004`. Approached door `20000` from its usable side; ordinary use changed it to opening and then open. Continued through the north wing and collected supplies.
4. **East hall:** Collected bullets and killed imps `31005`, `31006`, and elevated imp `31010`. Killed east/southern trooper `31007` while repositioning.
5. **Toxic channel / raised walkway:** The objective banner requested the raised walkway. Ordinary movement reached it: observed floor height changed from approximately `-1.48` to `-0.75` and then `-0.38`. The walkway, medkit, dead body, toxic channel, and southern door were visibly present. Evidence: `milestone-raised-walkway.png`.
6. **Southern study door:** Tried lower-floor approaches, side repositioning, the raised-walkway approach, and centering door `20001` in `aimHit`. Even at close range and correct visual aim, interaction remained `OutsideQuery`; ordinary `E/use` did not open it. Evidence: `attempt-door-high-angle.png`, `act-use-door-high-angled.json`.
7. **Lower east passage:** Tried door `20003` from the lower floor and alternate approach angles. Static collision prevented useful access and the door remained unavailable. Evidence: `attempt-lower-east-door.png`, `act-use-lower-east-door-outsidequery.json`.
8. **Recovery attempts:** Tried back/forward/left/right and diagonal movement around the toxic channel, raw Space, browser Space, and virtual gamepad `A` jump probes. These did not produce a semantic position or floor-height change that bypassed the blockage.
9. **Endpoint:** Preserved the exact stuck pose and captured final semantic state, targets, and a neutral screenshot. Stopped only the owned browser session. Evidence: `final-neutral-stuck.png`, `final-observe.json`, `final-targets.json`, `stop-receipt.json`, `post-stop-status.json`.

## Final observed blocker state

- Player position was approximately `(x=51.785564, y=-0.57999945, z=14.684308)`, floorY `-1.4799994`, yaw `7.700013`.
- `door-south-study` (`20001`) was still closed and reported available/usable by target inspection, but its interaction state was `OutsideQuery`.
- `room-south-terminal` remained available at distance about 22.44; `exit-hangar` remained available at about 25.42.
- `door-lower-passage-east` (`20003`) remained closed.
- The final screenshot showed the brown raised walkway/platform edge, a large wall, the open walkway with a body and medkit in the distance, and the green toxic channel on the right. HUD showed `SHELLS24`, `ARMOR75%`, `HEALTH9%`, `KILLS8/12`, `ITEMS8/16`.
- Route assistance returned `NoPath` for the southern study door. This was treated as a route limitation, followed by ordinary exploration and recovery attempts.

## Damage and jump uncertainty

- The exact damage source was **not identifiable from the available telemetry**. Health loss was temporally associated with movement through the toxic channel, and health stopped falling while on the raised walkway, so environmental hazard damage is likely. Combat also occurred during the run, so the evidence does not apportion the total loss between hazard and enemies per hit. No authoritative damage-source field was exposed in the observed state.
- Raw keyboard Space, browser Space, and virtual gamepad `A` were sent as bounded ordinary jump probes. The resulting observations showed no meaningful change in semantic position, floor height, yaw, or route progress. This means the jump attempts did not bypass the collision; it remains uncertain whether jumping is unsupported, unmapped in this action-driven mode, blocked by geometry, or simply too weak for the obstacle. No conclusion about the product's intended jump behavior is claimed.

## Evidence index

All paths are under `/tmp/doom-luna-campaign/run-3/` unless stated otherwise.

- `initial-neutral-42c87d45.png` and `initial-neutral-42c87d45.json`
- `milestone-shotgun-kills1.png`
- `milestone-raised-walkway.png`
- `attempt-door-high-angle.png`
- `attempt-lower-east-door.png`
- `survey-south-blocker.json`
- `survey-south-second.json`
- `survey-final-lower-wall.json`
- `final-neutral-stuck.png`
- `final-stuck-contact.png`
- `final-stuck-survey-0.png` through `final-stuck-survey-7.png`
- `final-observe.json`
- `final-targets.json`
- `final-capture-receipt.json`
- `act-use-door-high-angled.json`
- `act-use-lower-east-door-outsidequery.json`
- `post-stop-status.json`
- `stop-receipt.json`

The original final capture was also retained at:
`/home/agent/.local/state/crew-playtest-local/slots/slot-3/browser/2c045a19-4ddd-43b0-80d7-2a2bf4137b61/becc8fd3-86bf-4bdc-90e8-30b98e04e936.png`

## Assistance used

Used `discover`, `observe`, `targets`, `route`, `act`, `look`, `survey`, and `capture`, plus ordinary WASD, `E/use`, attack, look, raw keyboard Space, and virtual gamepad jump probes. Semantic assistance influenced targeting and door decisions; it is evidence of assisted operation rather than unaided visual discovery. No source, product, configuration, gameplay mutation, teleport, cheat, or alternate harness was used.

## Ranked improvement suggestions

### Game

1. Make the raised-walkway transition and southern door interaction volume clearly reachable from the required side and elevation.
2. Provide a safe traversal route when the objective explicitly requires the raised walkway.
3. Clarify alternate entrances and lower-passage affordances.

### Tools

1. Return an actionable waypoint or recovery hint when `route` reports `NoPath`.
2. Expose interaction rejection details such as side, elevation, distance, and occlusion.
3. Document or support vertical movement in the ordinary action catalog.

### Telemetry

1. Expose the live interaction rejection cause and candidate entity.
2. Report collision surface, floor height, hazard volume, and damage source.
3. Correlate screenshots, semantic frames, and action IDs, and include useful details for debug 422 responses.
