# Doom Luna independent playtest — run 2

Date: 2026-09-27 (session capture timestamps are UTC 2026-09-28)

## Session and outcome

- Profile: `doom-luna-2`; URL: `http://127.0.0.1:4492/`; native host: `4492`.
- Session: `09f620c3-3f45-4e39-b742-37c1bcf240c9`, slot 2.
- Events: `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/09f620c3-3f45-4e39-b742-37c1bcf240c9/events.jsonl`.
- Operational result: browser launched, rendered, accepted movement/look/fire/use input, and emitted captures.
- Mission result: **incomplete / uncertain**. Kills remained `2/12`; the north-wing door could not be opened after repeated approaches and `E`/gamepad use attempts. I stopped after bounded recovery attempts because no further progress was observable in this session.
- Model identity: not supplied by the profile or authoritative runtime metadata. The profile/session values above are the available runtime identity.

## Final state and route

- Latest visible/query state: position approximately `(x 7.499, y 0.920, z -32.308)`, facing the north-wing door; health `100%`, armor `100%`, shotgun equipped, bullets `86`, shells `15`, items `5/16`, kills `2/12`.
- Final door pose: yaw `82.5°`, pitch `2.9°`; `aimHit` was entity `20000` (`north wing door`), distance `0.607m`, range `70`. The accepted `KeyE` use action moved only `0.131m`; the resulting interaction was `reason: OutsideQuery`, `selected: null`, stamp `generation:1;step:4349;gameplay:26`, while the door stayed `closed`, `raised:0`.
- Remaining target count: HUD target stayed `12`; latest live enemy observations still showed multiple alive enemies. No death occurred.
- Route: starting loading bay -> central pickup lane -> west corridor/shotgun alcove -> central/north passage -> courtyard and industrial-room edges -> repeated approaches to `door-north-wing`.
- Collected: starting bullets, armor, west shotgun, west shells, and the farther bullets pickup. The nearby stimpack was left because health was full; southern supplies and remaining enemies were not reached.

## Direct evidence

The original files remain in the session directory; compact copies are in this run directory.

- Initial neutral scene: artifact `a54ed546-0124-415f-9c37-920e79756087`; original `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/09f620c3-3f45-4e39-b742-37c1bcf240c9/a54ed546-0124-415f-9c37-920e79756087.png`; copy `initial-a54ed546.png`.
- Initial HUD showed loading bay, health 100%, bullets 50, armor 0%, pistol, kills 0/12, items 0/16.
- Combat/loot captures: `door-shot-4fc943f9.png` (shotgun fired at the closed door), `blocked-corridor-2ddfad93.png`, `courtyard-769ff30b.png`, and `industrial-room-ff165e88.png`.
- Final failed-use capture: artifact `617c1c60-d088-4842-8c82-f7ad8da783b8`; original `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/09f620c3-3f45-4e39-b742-37c1bcf240c9/action-617c1c60-d088-4842-8c82-f7ad8da783b8.png`; copy `final-blocked-door-617c1c60.png`.
- Final neutral observation: artifact `2c51fa34-962c-4d96-982a-71438a734ff1`; original `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/09f620c3-3f45-4e39-b742-37c1bcf240c9/2c51fa34-962c-4d96-982a-71438a734ff1.png`; copy `final-observe-2c51fa34.png`. It shows the gray wall/door panel filling the view, shotgun centered, and HUD shells 15, armor 100%, health 100%, kills 2/12, items 5/16.

## Compact action/observation receipts

1. `discover` and action-driven `time` succeeded; the supplied controls were ordinary `WASD`, `E`, `Ctrl`, weapon keys, and `J/L`/`I/K` look controls.
2. Forward movement collected bullets, then pistol aim/fire killed enemy `31001`; a second aim/fire sequence killed `31002`. HUD advanced to `2/12` and health stayed 100%.
3. Movement through the west side collected armor, shotgun, shells, and a second bullets pickup. A visual capture showed the courtyard and another showed the industrial-room edge, proving the session was rendering and movement had spatial effects.
4. Route assistance returned `NoPath` for physically reachable west/courtyard targets. I treated that as a hint and verified movement and pickups with fresh observations.
5. At `door-north-wing` entity `20000`, repeated approaches placed the aim hit at about `5.86m` (position `(3.302,-29.744)`, yaw `55°`), `1.06m` (`(7.229,-32.732)`, yaw `55°`), `0.30m` (`(7.824,-31.821)`, yaw `67°`, pitch `2°`), `0.74m` (`(7.369,-32.290)`, yaw `82.5°`, pitch `2.9°`), and finally `0.61m` (`(7.499,-32.308)`, yaw `82.5°`, pitch `2.9°`). Every live observation reported `OutsideQuery`; the door remained `closed`/raised `0`.
6. Distinct recoveries were bounded and had visible position effects: I backed from the first close approach to `(3.044,-31.935)`, strafed to `(4.705,-33.173)`, re-approached to `(7.830,-33.198)`, then strafed until movement saturated around `z=-33.73`; I turned away and moved west to `(-0.714,-30.829)`, traversed the courtyard/industrial-room edge to about `(-1.73,-4.35)`, returned through the loading bay, and came back via `(1.512,-22.006)` and `(4.447,-31.500)` before the final door approach. I also tried pitch/yaw changes, one shotgun shot at the door, keyboard `E`, gamepad `X`, ordinary `use` actions, and a raw held movement/jump input. These produced no door transition. The final observation was captured after the last use attempt.

## Friction and suggestions

### Gameplay

1. **North-wing door affordance:** the crosshair hit entity `20000` from sub-meter range, but `E`/use did not change its closed state and reported `OutsideQuery`. Provide a visible/authoritative reason or make the intended door-use condition discoverable.
2. **Route continuity:** several walls/corridor edges stopped movement while the target route reported `NoPath`; a reachable-room map or an obvious alternate opening would make recovery possible.

### Harness

1. The capability discovery output advertised `interaction.help`, `interaction.inspect`, and `interaction.use`, but invoking those operations returned an unknown inspection operation; `playtest interaction` also reported that this profile was not opted into product interaction queries. The ordinary action path still ran, but the missing diagnostics made the door failure hard to distinguish.
2. Action-driven captures and event receipts were useful: they preserved original image paths, target IDs, movement, and HUD changes. Keep this evidence path available for future trials.

### Missing facts

1. The product exposed no authoritative blocked-door reason in the available profile. `OutsideQuery` is insufficient to tell whether the door needs a key, a different facing/position, an objective, or a product bug.
2. No collision/navigation query was available, so `NoPath` could not be reconciled with the observed traversal without manual visual exploration.

## What worked and next tactic

- Worked: fresh native session, neutral initial capture, action-driven time, live observations, ordinary keyboard/gamepad input, visual movement through multiple spaces, pickup collection, pistol combat, and kill/HUD deltas.
- Next tactic if the product/harness is updated: expose the door's required state/reason and a reachable route to the southern/terminal areas; then resume from a fresh session and clear remaining targets. In the current session, further progress was not observable after several distinct recoveries, so continuing would be repeated no-op input.

## Tools and cleanup

- Used only the supplied `/home/agent/.local/bin/playtest` CLI: `games`, `start`, `assist discover`, `assist time`, `assist targets`, `assist route`, `assist action`, `observe`, `act`, `look`, raw keyboard/gamepad input, and `stop`.
- No source inspection, product edits, cheats, direct mutation, teleport, custom script, alternate harness, or parent route hint was used.
- Original captures and this receipt are retained under `/tmp/doom-luna-campaign/run-2`.
- Cleanup: stop returned `browser_closed:true`, `released:true` for slot 2; a subsequent status check showed slot 2 unoccupied. Parent-owned slots 1 and 3 were left running.
