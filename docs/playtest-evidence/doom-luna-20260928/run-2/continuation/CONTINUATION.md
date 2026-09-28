# Diagnostic-assisted Doom continuation

This is a separate report for the diagnostic-assisted continuation. It does not
replace the independent trial report at `../EXIT_INTERVIEW.md`.

## Session and outcome

- Profile: `doom-luna-2`; product URL: `http://127.0.0.1:4492/`; native host: `4492`.
- Profile configuration had `interaction_queries: true`.
- First continuation browser session: `192cdf17-1fbe-4bf3-993b-a3e3ea884e49`, slot 1, created `2026-09-28T04:32:35Z`. It later became CLI-inaccessible even though aggregate status still reported it connected.
- New continuation session used for the run: `0dea4ce1-cd46-44ea-b1d0-f16dc97b35ee`, slot 2, created `2026-09-28T04:41:06Z`.
- Events: `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/0dea4ce1-cd46-44ea-b1d0-f16dc97b35ee/events.jsonl`.
- Runtime model identity: not supplied by the profile or authoritative runtime metadata.
- Operational result: the browser rendered, accepted ordinary movement/look/fire/use input, and preserved the retained native-host world.
- Mission result: **incomplete / uncertain**. The run stopped at `8/12` kills with the player alive at `18%` health. Four hostiles remained, and the southern traversal was blocked after distinct recoveries.

## Final state and route

The continuation began from the retained world at step `4890`, position
`(42.9536, 0.4200, -25.9567)`, yaw `124.5°`, pitch `-0.1°`, health `100%`,
bullets `86`, shells `29`, shotgun equipped, and `4/12` kills. The north-wing
door was already open from the earlier diagnostic-assisted portion.

The ordinary route reached the east hall and industrial room. It defeated
`31005` (imp), `31006` (imp), `31010` (imp), and `31007` (trooper), advancing
from `4/12` to `8/12`. The run collected the bullets pickup `30010`; stimpack
`30011` remained visible in facts but could not be reached from the collision
corner. The final HUD showed shotgun, `106` bullets, `20` shells, `77%` armor,
`18%` health, `9/16` items, and `8/12` kills.

Final live product facts were position `(46.2700, -0.5990, 2.0466)`, feet
height `-1.4990`, yaw `-136.0°`, pitch `-0.35°`, with a static mesh `0.389 m`
away on the aim ray. The north door (`20000`) was open. Southern study
(`20001`), terminal study (`20002`), and lower passage (`20003`) remained
closed.

Remaining alive targets at the final observation:

- `31012` imp, health `60`, position `(39, -1.75, 10)`, distance `10.82`,
  sleeping and `lineOfSight:false`.
- `31008` imp, health `60`, position `(54, -0.75, 27)`, distance `26.12`,
  sleeping and `lineOfSight:false`.
- `31011` trooper, health `30`, position `(26, -1.75, 11)`, distance `22.18`,
  sleeping and `lineOfSight:false`.
- `31009` trooper, health `30`, position `(54, -0.75, 39)`, distance `37.75`,
  sleeping and `lineOfSight:false`.

## What the newly available diagnostics explained

The profile opt-in did not produce a usable `playtest interaction` query:
both the default call and a reticle-mode call returned
`playtest: capability_unavailable: requested product debug query`. The live
product catalog was HTTP 200 and advertised `interaction.help`,
`interaction.inspect`, and `interaction.use`, but it did not expose the
generated query/cursor operations expected by the playtest CLI.

I used the product's read-only debug transport as a bounded fallback:
`POST /__rusty/product/runtime/debug/execute` with `text/plain` command bodies
`interaction.help` and `interaction.inspect`. I did not invoke
`interaction.use` and did not mutate a target through diagnostics.

The fallback explained and resolved the old north-door problem. At the retained
north pose, door `20000` was available and visible at `0.601 m`, but the query
focus was `OutsideQuery` because the pitch was wrong. Ordinary `assist look`
pitch correction selected door `20000` (`Ready`, `aimHit.entity:20000`), and
ordinary physical `E` opened it. The downstream observation confirmed the door
state changed to open and the player entered the north wing.

It also made the southern blocker concrete. At a close lower-door approach,
the read-only facts were:

- pose: position `(50.9516, -0.5894, 6.9891)`, yaw `179.0°`, pitch `-36.35°`;
- door `20003` point `(51.1, -0.5894, 8)`, distance `1.022 m`, reach `2.5 m`,
  availability `Available`;
- visibility `Occluded`, `targetedUseReason: Occluded`, selected target null,
  focus `OutsideQuery`, aim hit a static mesh at `0.759 m`.

The ordinary `E` use at that pose was accepted as a physical key submission,
but moved only `0.131 m`; the door stayed closed and the live interaction
reason remained `Occluded`. This is the direct refusal evidence requested for
the blocked position.

At the final position, the south-door query still reported door `20001`
available but `Occluded`, `OutsideQuery`, and `14.715 m` away. The authored
route query for `door-south-study` returned `NoPath`, no next waypoint, and
`requiredAction.key: E` with `eligibleNow:false`. The terminal-room route also
returned `NoPath`. These facts explained why combat could not continue: every
remaining hostile had `lineOfSight:false` while the player was wedged against
static geometry.

## Bounded recovery attempts

I alternated fresh observations with distinct ordinary-control attempts rather
than replaying one encounter script.

1. Backed away from `(51.99, 7.09)` for `600 ms`: moved `3.58 m` to about
   `(49.81, 4.24)`, with health falling to `58%`.
2. Reoriented toward the stimpack and advanced: reached `(51.99, 5.41)`;
   the next forward hold produced `0 m` movement and `no-observed-movement`.
3. Strafed right to `(50.07, 7.33)`, then left to `(50.89, 4.76)` and
   forward back to `(51.98, 5.42)`. The stimpack stayed uncollected; health
   reached `46%`.
4. Corrected yaw/pitch toward the lower door, captured `Occluded` facts, and
   pressed physical `E`. The door remained closed; the refusal capture is
   `south-door-use-refusal-6e598855.png`.
5. Restored a level pitch, backed to `(50.82, 2.32)`, strafed left to
   `(52.89, 1.74)`, and moved forward toward the visible southern door. The
   player advanced to about `z=3.99`, then the next forward hold returned
   `0 m` movement. A further side attempt reached `(53.78, 2.67)` but another
   forward hold moved only `0.198 m`; health was `26%`.
6. Circled right through `(50.20, 2.69)` and `(46.27, 2.76)`, turned toward
   the remaining imp, and advanced to `(46.27, 7.33)`. The aim ray still hit a
   static mesh at `0.375 m`, and the imp remained out of line of sight.
7. A final right/forward recovery, followed by one bounded raw `W+D` hold and
   `700 ms` advance, produced no further movement. Health remained `18%`.

The final neutral screenshot shows the player pressed against a tall brown
wall beside the toxic green pool, with no visible route to the closed south
doors. I stopped because further input was repeating a collision no-op with
four targets still hidden behind the blocked traversal; this is a genuine
blocker, not a victory or player death.

## Direct evidence

- Initial continuation scene, source:
  `/home/agent/.local/state/crew-playtest-local/browser/192cdf17-1fbe-4bf3-993b-a3e3ea884e49/8856205a-f3cc-47d9-ac25-2117c0ec520f.png`;
  compact copy: `initial-8856205a.png`.
- North-door diagnostic success, source:
  `/home/agent/.local/state/crew-playtest-local/browser/192cdf17-1fbe-4bf3-993b-a3e3ea884e49/action-83b0d39f-bced-487c-a9a0-a8e5397efff0.png`;
  compact copy: `north-door-open-83b0d39f.png`.
- North-wing entry after opening, source:
  `/home/agent/.local/state/crew-playtest-local/browser/192cdf17-1fbe-4bf3-993b-a3e3ea884e49/action-6a1d9da9-4a68-426b-8432-3085787edfab.png`;
  compact copy: `north-entry-6a1d9da9.png`.
- South-door E refusal: `south-door-use-refusal-6e598855.png` and its original
  source under the slot-2 browser directory.
- Final neutral scene: artifact `87d9a0e9-c10a-4454-8655-d22ca13b5128`, source
  `/home/agent/.local/state/crew-playtest-local/slots/slot-2/browser/0dea4ce1-cd46-44ea-b1d0-f16dc97b35ee/87d9a0e9-c10a-4454-8655-d22ca13b5128.png`,
  compact copy `final-87d9a0e9.png`.
- Read-only interaction receipts: `interaction-inspect-south-approach.json`,
  `interaction-inspect-lower-door.json`, `interaction-inspect-blocked-pose.json`,
  and `interaction-inspect-final.json`.
- Route and final live-state receipts: `final-route-south-study.json`,
  `final-assist-observe.json`, and `final-observe-receipt.json`.
- Full original event journal: `events-original.jsonl`; compact selected
  action/observation rows: `receipts-selected.jsonl`.

## Friction and ranked suggestions

### Gameplay

1. Make the lower-passage/southern-door approach physically traversable from
   the east hall, or expose a visible alternate opening. The door was available
   and within reach, but a static mesh consistently occluded its use ray and the
   player could not cross the nearby collision corner.
2. Keep reachable supplies reachable from the same floor. The stimpack was as
   close as `2.10 m` during the approach, but the surrounding collision left no
   ordinary path to collect it while health fell.

### Harness

1. Register the generated interaction query/cursor operations when
   `interaction_queries:true` is enabled. The profile advertised opt-in, yet
   `playtest interaction` returned `capability_unavailable` and the live catalog
   omitted the expected query operations.
2. Provide a reconnect/stop path for an occupied session whose client lease is
   stale. Session `192cdf17-...` remained listed as connected but every direct
   session operation returned unknown-session.

### Missing product facts

1. Return a collision or authored-gate reason with `NoPath`; the route facts
   currently say `NoPath` with no waypoint while the player can still see some
   target geometry.
2. Return an authoritative use refusal reason that distinguishes occluding
   static geometry, wrong approach side, and a closed-door requirement. The
   interaction facts identify `Occluded` but do not identify which surface or
   route correction is intended.

## What worked and next tactic

The retained world, action-driven time, ordinary keyboard movement/look/fire/use,
live combat facts, target/route facts, and original screenshots all worked. The
new read-only interaction facts directly explained the north-door pitch issue
and enabled the ordinary `E` transition. Four remaining enemies and unopened
southern/terminal areas show that progress remained possible in principle, but
this session had no observable progress left after the distinct collision
recoveries. A next run should first use the interaction facts to find a
traversable approach to `20003` or `20001`, then restore line of sight before
spending shotgun shells.

## Tools and cleanup

Used only the supplied `/home/agent/.local/bin/playtest` CLI and the product's
read-only debug HTTP endpoint: `games`, `status`, `assist discover`,
action-driven `time`, `observe`, `targets`, `route`, `act`, `look`, `advance`,
ordinary raw keyboard input, captures, and read-only `interaction.help`/
`interaction.inspect`. No product source inspection, code edit, profile edit,
cheat, direct mutation, teleport, or alternate harness was used. The current
slot-2 browser session was stopped after this report was written with
`browser_closed:true` and `released:true`. A subsequent stop of the earlier
slot-1 session also succeeded with the same receipt, and final pool status
showed `active_count:0`; no browser sessions from this continuation remain
occupied.
