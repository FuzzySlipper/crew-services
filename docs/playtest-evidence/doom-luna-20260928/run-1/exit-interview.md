# Doom Luna independent playtest: exit interview

## Result

**Incomplete / uncertain mission outcome.** The player was still alive when I stopped at the practical 25-minute ceiling. The level did not reach all-enemies-eliminated or player-eliminated.

- Kills: **2 / 12** (the HUD target count was 12; the final semantic snapshot had 8 alive entries after two dead entries).
- Final player state: **96% health, 99% armor, 90 bullets, 12 shells, shotgun equipped and ready**.
- Pickups: **6 / 16**. Collected the western shotgun, shells, stimpack, armor, and two bullet pickups.
- Doors: all four observed doors remained closed, including north wing (entity 20000).
- Stopped after distinct visual recovery attempts and a bounded door-interaction reproduction. I did not exhaust the 300-action cap; an exact action count was not retained.

## Route and rooms visited

Spawn/loading bay -> west corridor and western shotgun/shell pickup area -> stimpack corridor -> central pit room -> north-wing doorway -> central room -> lower outdoor/slime-pit courtyard -> lower-floor pocket near the central room. I could not reach the southern study, terminal, or east/lower passage rooms.

## Evidence

All worker evidence is under `/tmp/doom-luna-campaign/run-1`:

- `initial.png`, `initial.json`: neutral opening view, 0/12 kills, 100 health, 50 bullets.
- `milestone-kill-1.png`, `after-kill-1.png`, `after-kill-1.json`: western encounter and first confirmed kill.
- `north-door-probe.png`, `north-door-probe.json`: original screenshot/metadata at the north door after focus and use attempts.
- `survey-contact.png`, `survey.json`: lower-floor visual survey and restored position.
- `final.png`, `final.json`, `final-assist-observe.json`: final visual and semantic state.
- `final-targets.json`, `final-route-shotgun-south.json`: final target list and route result (`StartNotWalkable`).
- `cleanup-stop.json`, `cleanup-status.json`: session cleanup receipt and post-stop status (written after stopping).

The session was `a9f4b887-1bd3-46ce-993b-1755a6047688`, profile `doom-luna-1`, slot `slot-1`, on the supplied local service. The final observe also recorded three 422 responses from the unavailable product debug execute route and GPU ReadPixels stall warnings; there were no page errors.

## Assistance and controls used

Used `assist discover`, action-driven time, `assist observe`, `assist targets`, `assist route`, `assist act`, corrected `assist look`, `assist survey`, ordinary keyboard input, raw relative-mouse input, `browser inspect`, and final captures. `playtest interaction` was unavailable because the profile did not opt in, and `rusty-live-debug` was not installed. No product source, cheat, teleport, direct damage, or substitute harness was used.

## Friction incidents, ranked

1. **Gameplay/product: door interaction did not follow the visible target.** At approximately x=7.2, z=-32.5, yaw 90 degrees, the crosshair ray hit door entity 20000 at about 0.9-1.9 m. `assist act use`, raw E, raw F, canvas focus/pointer lock, and a longer 800 ms use all left semantic interaction as `OutsideQuery` with `selected:null`; `raised` stayed 0 and the door stayed closed. The HUD says “F opens doors” while the assist action map describes use as E, creating a control-contract conflict. The door-probe image shows the wall/door filling the view, so this was not simply a distant target.

2. **Gameplay/navigation: collision and floor state trapped the player.** The lower courtyard had a lower floor around y=-0.5. Forward, backward, strafing, and diagonal probes moved along the pocket boundary or stopped at collision, then returned to the same area. Repeated probes covered roughly x±1.73 and z=-4.3 through -13.7 without finding an ascent or exit. The route helper reported `StartNotWalkable` from that position even though the scene was visibly rendered and ordinary movement remained possible.

3. **Tool/telemetry: semantic helpers could not explain or drive the state.** Several route requests returned `NoPath`/`StartNotWalkable` while visual scenes had apparent corridors. The interaction command was unavailable, and no alternate read-only debug command was installed. Accepted actions reported input transport but did not prove the downstream door transition. The final capture also retained 422 debug-execute errors.

4. **Tool/input focus: pointer lock was coupled to an accidental weapon action.** Clicking the canvas acquired pointer lock, but the click also fired the shotgun and consumed a shell. One aim attempt hit a static wall and consumed another shell without a damage event. Focus was therefore established, but it was not a clean interaction proof.

5. **Camera/aim clarity: orientation and focus were separate from target selection.** At the north door I expected yaw 90° plus forward to face east into the doorway; the observed yaw was 90° and the visual crosshair/ray hit entity 20000, but selection remained null. After correcting the documented `assist look` fields to `yaw`/`pitch`, cardinal movement behaved consistently: yaw 90° forward moved +x, yaw 0° forward moved -z, and yaw 180° forward moved +z. The pointer-lock state became true after canvas focus. This separates camera orientation and input focus from the missing interaction selection/transition.

## What helped

Short movement/action probes with a screenshot after meaningful changes, semantic health/ammo/pickup observations, and the visual survey were useful. The stimpack and armor pickups restored/extended survivability. Physical keyboard input and relative mouse input moved the player once pointer lock was established.

## What I would try next

First expose or repair the authoritative door-use binding and selected-target telemetry, then verify one before/after door transition from the same position. Start the next route from a walkable upper floor, avoid dropping into the lower pocket before the exit is known, and capture the aim ray, selected entity, and door state together. A fresh run with that interaction path should have enough health and ammunition to continue through the southern/eastern rooms.

## Was more progress possible?

**Yes, conditionally.** The player had ample health, armor, and ammunition, and the semantic snapshot listed distant living enemies and pickups. Further exploration appeared possible if the north-door interaction or another valid route out of the central/lower area became usable. From this session's final pocket, additional ordinary movement probes were unlikely to add progress without resolving the collision/route or door-selection mismatch.
