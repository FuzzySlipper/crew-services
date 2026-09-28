# Doom capsule clearance and movement follow-up — 2026-09-28

## Changes

- `assist clearance` reports the current player capsule overlap, a direct sweep
  toward nearby target feet, target overlap, and a short support probe. It uses
  Engine's existing capsule queries and Doom's current door collider records.
  This is inspection, not a pathfinder or a promise that a jump will succeed.
- Engine capsule queries now ignore disabled colliders and triggers, matching
  ordinary controller inputs. An opened door must not remain a phantom blocker.
- The controller preserves motion along a feasible shared crease when three or
  more side contacts are present. The previous unconditional zero discarded
  vertical escape even when all side normals were horizontal.
- Missing route IDs now produce a request example before dispatching debug input.

No teleport, backwards time, alternate movement solver, or scripted clear was
added. Queries stay triggered rather than becoming continuous telemetry.

## Regression evidence

`side-contact-repro.log` and `capsule-filter-before.log` show the focused tests
failing before their respective fixes. The final spatial crate and native bridge
suites pass, as do 108 browser-host tests and the managed SDK build. The airborne
steep-ramp integration case passes, but also passed before the correction; it is
not an exact reproduction of the earlier Doom snag. Three independent review
lanes found no remaining issue after the collider filtering correction.

The development package is SDK c, with final runtime k. See `environment.json`
for identities. Runtime i is the comparison build with the new inspection helper
but without the two native fixes. Both browser trials use independent native worlds
and GPT-5.6 Luna with max reasoning. Their routes are chosen by the testers, so the
runs are not an exact replay comparison.

## Evidence limits

The Engine warning-delta script launches its own browser; these required
exercises used the managed crew-services playtest sessions. A compatible warning
baseline was not captured. No clean warning delta is claimed. Browser diagnostics
in the tester receipts are observations, not a certified delta.

CoreCLR is the development acceptance lane. NativeAOT execution of the debug
helpers is outside this follow-up's proof.

## Engine CI

Gate 3735 passed at `61f9f6ebea2e968f42d2182dcd381ef5fbba02e6`:
verify-rust, verify-csharp, verify-render, and verify-docs-and-routing. Fresh Den
readback confirmed `passed` with evidence posted; see `engine-ci-gate.json`.

## Baseline Luna trial

Runtime i finished at **8/12 kills, 59 health, alive and grounded**, after
19m51s and 124 recorded requests. The north door opened with ordinary E. Physical
jumps recovered onto the raised walkway. The old frozen-airborne pose did not
recur, so this trial does not reproduce the original defect. The run stopped at
the time bound, before the southern doors.

See [baseline exit interview](baseline-luna/EXIT_INTERVIEW.md) and
[final original capture](baseline-luna/final-15e9ab7a.png). Interview paths under
`/tmp` identify the original files; the same filenames are preserved alongside
the interview here. Reports are retained as written by the testers.

## Deployment and backup

Doom commit `0a881b278bfe36960be4f9c92ab90600e3d17460` is pushed. The default
4394 host uses SDK c and final runtime k; startup and HTTP readiness are verified.
Build, lifecycle exercise, semantic catalog check, and boundary audit pass.
Doom and Crew have no configured GitHub workflow here, so no CI gate was invented
for those repositories.

## Final-runtime Luna trial

Runtime k finished at **6/12 kills, 36 health, alive**, with time held at step
7405. It opened the north door, reached the east and southern walkways, and used
physical jumps. The final capture shows the player beside a wall in toxic waste.
The trial stopped before clearing the level or dying. The intended 20-minute
bound was exceeded while wrapping up; the final receipts cover approximately
23 minutes. It is not an equal-duration performance comparison.

Opened-door clearance returned no target overlap. The query left step 5211
unchanged. Starting floor contacts remained visible in the sweep, so the query
needs interpretation rather than supplying a ready-made movement instruction.
See [final-runtime exit interview](final-luna/EXIT_INTERVIEW.md) and
[original final capture](final-luna/final-neutral.png).

Both owned browsers have stopped; pool readback reports zero active sessions.
The temporary 4495/4496 hosts and profiles were removed, preserving the updated
default 4394 host.

## Follow-up priorities

1. Distinguish supporting floor contact from an obstacle ahead in clearance
   presentation, while keeping raw collision facts accessible.
2. Add surface/height context for nearby raised walkways and hazard floors to
   make ordinary jump target selection easier.
3. Continue focused attempts at the original snag; neither these independent
   routes nor the generic ramp regression establish an exact reproduction.

## Interview interpretation notes

The final tester's phrase “resolved the prior snag” means it traversed nearby
geometry; it did not reproduce and then release the original frozen pose. Its
20-minute-ceiling wording describes the intended bound, not an exact duration.
The tester left its native host for the parent, who subsequently stopped it.

The final browser captured a debug HTTP 422 without a correlated request body.
The interview's guessed map-request cause is unverified and is not accepted as
an established explanation. The session remained usable with no page errors.
This diagnostic provenance gap is retained as follow-up evidence for task 8716;
no clean-warning claim is made.
