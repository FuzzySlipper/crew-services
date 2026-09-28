# Doom spatial playtest follow-up — 2026-09-28

## Scope

Triggered Engine collision grid and nearby rays; live jump input guidance;
interaction query wiring and angle guidance; Doom eye-height door focus,
character-step diagnostics and recent damage source. No reverse time, second
movement solver or automatic playthrough script.

## Verification

- Crew browser action tests: 5 pass, including partial release failure.
- Focused Go session/browser/client/CLI tests: pass.
- Engine product-browser-host: 107 tests pass.
- Engine interaction exercise: pass, including signed look guidance and grounded/
  height refusal for jump planning.
- Doom builds against immutable SDK b and runtime h development artifacts.
- Runtime trust and Engine reuse reviews: no findings.
- Scope is the CoreCLR development lane. Runtime execution of the new debug JSON
  helpers under NativeAOT has not been established.

The prior listed-but-unknown session issue did not reproduce in a fresh second
slot through list status, per-session status, profile reload, browser inspection
and cleanup. See `session-consistency.json`. This does not establish its cause;
no speculative session-lifecycle fix was made.

## Live evaluation

Independent GPT-5.6 Luna (max reasoning) finished at **7/12 kills, 72 health,
alive**, after 121 actions. It opened/crossed the north door with ordinary E and
used a physical jump to recover from toxic flooring. A later jump stopped on a
steep-slope contact with `grounded:false`; subsequent advancement did not move
it. The level was not cleared, and the player did not die.

Grid/probe calls at the actual blocker left simulation step 5272 unchanged.
The occupancy view helped expose nearby collision bands, and ray/character
receipts identified wall and steep-slope contact. They did not establish a
player-sized traversable route. The direct yaw/pitch guidance was useful at the
door; the grid alone did not solve aiming or navigation.

See [exit interview](luna/EXIT_INTERVIEW.md),
[north-wing screenshot](luna/milestone-inside-north-wing.png),
[blocker screenshot](luna/milestone-toxic-blocker.png), and
[jump recovery](luna/milestone-jump-recovery.png).

The interview is retained as the tester wrote it. Its phrase “read-only advance”
is incorrect: `advance` mutates simulation time; grid/probe/inspection are reads.
The tester's model identity is recorded here and in environment.json, separately
from the game's runtime metadata.

## Final correction and deployment

The product reviewer identified the plain jump action's fixed 700 ms duration.
It now resolves MoveMs+SettleMs from live controller tuning; targeted rereview
approved. The final smoke returned 766.67 ms, accepted ordinary jump input and
finished grounded. Build, lifecycle exercise and boundary audit pass.

An immediate post-launch smoke initially saw the inspection adapter unavailable;
that session was stopped. The final smoke waited for read-only discovery before
issuing actions. Both receipts are preserved.

The default rusty-doom profile now enables interaction queries and its local
4394 host runs runtime h/SDK b. The temporary 4494 host/profile and all owned
browser sessions were cleaned up. Unrelated profiles/work were preserved.

Engine gate 3734 passed at 1ae718f10bb0e53335512f2f2ffffa1182318dbb:
verify-csharp, verify-render and verify-docs-and-routing; authoritative readback
reported evidence posted. See `engine-ci-gate.json`.

## Next focused improvement

Reproduce the steep-slope stuck-airborne state with the retained pose and normal
controller inputs. Then consider a triggered player-sized clearance/surface
query using existing Engine collision mechanisms. Avoid turning the occupancy
grid into a second pathfinder. The tester initially used the wrong route request field despite the documented
`id` example; an actionable missing-id error would reduce that friction.
