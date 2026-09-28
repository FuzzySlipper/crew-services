# Doom Luna exploration trials — 2026-09-28

Three independent GPT-5.6 Luna playtester agents (max reasoning) used isolated
native hosts and local browser sessions. Each began at full health with zero kills.
They chose routes and combat actions from observations. No game code was changed,
no parent drove their controls, and no Jev or prescripted encounter was used.

The mission was to eliminate all 12 enemies or die. All three original trials
stopped alive at a blocker or practical time ceiling; none cleared the level.
The ceiling was 25 minutes / 300 actions, with extra time offered to run 3 if it
was still progressing. Counts are observed HUD/product facts, not precise runtime
benchmarks. Runtime g contains the tested behavior; Engine's later commit only
formats Rust. See campaign.json for revisions.

| Trial | Kills | Health | End condition |
|---|---:|---:|---|
| 1 | 2/12 | 96 | Time ceiling; north-door focus failure and lower courtyard traversal loop |
| 2 | 2/12 | 100 | Repeated north-door approaches could not establish interaction focus |
| 3 | 8/12 | 9 | Opened north door; blocked around southern walkway/passage after losing health |

Each trial has an exit interview, screenshots and receipts in its run directory.
Raw worker reports retain their observations and uncertainty; the synthesis below
separates confirmed evidence from proposed diagnoses.

## Findings to follow up

1. **Expose usable interaction diagnostics by default (crew-services).** The
   temporary trial profiles omitted interaction_queries, so the existing
   `playtest interaction` tool refused queries. Discovery also listed native
   debug commands which were not callable as assist operation names. This is
   partly a parent setup omission. Enabling the flag still left the query
   unavailable: the generated operations expected by the CLI were absent from
   the live catalog. The agent used the existing read-only debug HTTP endpoint
   instead. Make discovery distinguish callable assist operations from native
   catalog entries and wire the existing inspection facts through the ordinary
   tool. Avoid creating duplicate product semantics.
2. **Align door focus with the visible usable surface (Doom, with Engine inspection
   if necessary).** Two runs ray-hit the north door at close range but got
   OutsideQuery. A parent read-only query after run 2 stopped showed the door
   visible, available and 0.601 m away; targetedUseReason was Ready, but ordinary
   focusReason was OutsideQuery. Its candidate point was at player-body y=0.920,
   below the eye origin y=1.640. Angle was 0.933 radians versus the acquire cone
   0.32. This explains the focus rejection in that sample and suggests reviewing
   the candidate point/volume and useful pitch guidance. It does not prove all
   door failures share one cause or authorize widening a global focus cone.
3. **Explain blocked movement and no-route results (Doom + Engine).** NoPath and
   StartNotWalkable often accompanied areas the agents could partially traverse.
   Surface/floor/step-height and next reachable region facts would support
   recovery. Investigate the north transition and southern toxic walkway with
   ordinary controls; do not invent a second pathfinder or claim a route exists.
4. **Publish jump and composed movement actions (Doom provider + harness).** Raw
   Space/gamepad probes were inconclusive while time was held. Agents need a
   discoverable action that couples the actual jump/movement input to a bounded
   simulation window and reports grounded/airborne or blockage facts. Preserve
   normal validation and input semantics.
5. **Add concise damage and refusal details (Doom telemetry).** Run 3 reached
   eight kills but could not apportion lost health between hazard and enemies.
   Report last damage source/reason and recent action refusal where available.
   Keep timestamp/frame uncertainty explicit without adding certification work.

## What already helped

Action-driven time let agents inspect between encounters. All three obtained the
shotgun, defeated enemies and collected supplies. Live target/health/ammo facts,
short actions and restored camera surveys were useful. Independent outcomes
2/12, 2/12 and 8/12 show capability but uneven usability; this is not a statistically
controlled model comparison.

## Interpretation cautions

Run 1 reported a possible F/E HUD mismatch; inspected door captures display
`E USE`, so that binding conflict is unconfirmed and should not be treated as a
verified defect. Its shot while acquiring pointer lock is a separate observed
input side effect; assist already focuses without a click.

The agents' request for exact screenshot/action correlation is retained in their
interviews. Modest sample metadata and clear errors are sufficient for this
harness; exact cross-subsystem timing proof remains outside scope.

## Diagnostic-assisted continuation

After the original trial, run 2 resumed its retained world with permission to use
newly exposed read-only interaction details. It used ordinary look adjustment
and physical E to open the north door, then reached **8/12 kills**, **18 health**,
77 armor, 106 bullets and 20 shells. It stopped alive at a southern traversal
blocker, with four remaining hostiles out of line of sight. No target-ID mutation
or teleport was used. This continuation is not a fourth independent trial.

The detailed query improved the result from 2 to 8 kills without gameplay changes.
It also distinguished the lower door's refusal: available at about 1.02 m, but
Occluded by static geometry. That is a different condition from the north door's
visible-but-outside-focus-cone state. The tester exhausted several approaches
while losing health and could not reach nearby healing supplies.

The continuation also reported one session listed as connected while per-session
operations returned unknown-session. It recovered with a fresh browser on the
same host/world and cleaned up both sessions. Treat that as an observed service
consistency issue to reproduce; the report's lease explanation is a hypothesis.

See [continuation interview](run-2/continuation/CONTINUATION.md),
[run 1 interview](run-1/exit-interview.md),
[run 2 interview](run-2/EXIT_INTERVIEW.md), and
[run 3 interview](run-3/EXIT_INTERVIEW.md).

## Follow-up order

1. crew-services: make ordinary interaction queries match Engine's current
   inspection surface; accurately advertise supported operations and actionable
   errors. Reproduce the listed-but-unknown session separately.
2. Doom: investigate the close-door focus point relative to eye height and usable
   surface; include candidate/refusal facts in the compact observation.
3. Doom and Engine: reproduce the lower-floor/raised-walkway transition using the
   preserved poses; improve grounded, collision and route-failure diagnostics.
4. Doom provider and harness: make jump/composed movement available in held time.
5. Doom: expose recent damage cause so agents can distinguish combat from hazards.

These are recommendations, not implemented fixes. Engine-owned geometry and
controller mechanisms remain upstream; Doom owns level layout and interaction
meaning. No new task campaign was created. All test browsers and the three owned
native hosts were stopped, temporary profiles removed, and original evidence
retained. Existing unrelated profiles and product hosts were preserved.
