# Concurrent parent / Jev Doom experiment (task 8384)

## Current follow-up: gamepad assistance (task 8386)

The later Luna + Jev gamepad run **completed two kills** in 15.479 seconds and
35 actions. It stopped with `two_kills_observed`, the configured goal threshold,
not because of death or the time limit. Budget was 60 seconds / 100 actions;
final player state was alive, 70 health, 62 bullets, 2/12 kills. More enemies
remained, but continued success was not tested. To continue the owned session,
inspect it and raise the cumulative kill threshold and corresponding goal before
starting another bounded interval; otherwise the old threshold stops immediately.

Use [doom-gamepad-parent.json](../configs/playtest/doom-gamepad-parent.json),
replacing its placeholder session and product URL. It combines `combat.observe`
with `spatial.map ascii 15 2`, finite standard gamepad tactics and concurrent
Luna guidance. Gamepad aiming used Engine sticky acquisition and bounded shot
correction; ordinary collision still blocked obstructed shots. This was one
successful bounded trial, not a comparison against the earlier keyboard runs.

Jev request latency was 169ms median (123–392ms range). Luna updates applied
after 17 and 31 actions, taking 7.202s and5.987s while Jev continued acting.
Navigation projection was unavailable in that run; collision/annotation maps
were available. Do not infer current projection support from this old snapshot.
Original config, transcript, result and image are in
`/home/dev/evidence/task8386/` (`luna-gamepad.*`, `combat-summary.json`, `report.md`).

Subsequent Engine tasks 8385/8387 fixed shared Perception mesh occlusion and
browser request-abort behavior; Doom was updated to pair
`0.1.0-dev.dafdca8732a8`. Packaged checks resolved recorded aborts without
suppressing warnings; existing ReadPixels warnings remained. Those checks did
not rerun the combat comparison. See `/home/dev/evidence/task8387/report.md`.

## Historical keyboard experiment (task 8384)

The initial concurrent-parent experiment showed navigation and an ordinary-control
kill, but no run completed its two-kill mission. The results below describe that
earlier keyboard-only setup, before the gamepad assistance follow-up above.

## Reproduce

Use `configs/playtest/doom-parent-feedback.json`, replacing `session_id` with an
owned, connected browser session and `product_url` with that profile's exact URL.
The Doom host needs `combat.observe` and `spatial.map` in its live debug catalog.
Start/reset the game through ordinary inputs, then run:

```sh
playtest-assist --url http://127.0.0.1:48284 \
  --config configs/playtest/doom-parent-feedback.json \
  --output /tmp/doom-feedback-new.jsonl
```

The example uses local den-router and `codex-gpt-5.6-luna` through Responses.
For Sol change `parent_model` to `codex-gpt-5.6-sol`. For GLM use `glm-5.3` and
`parent_protocol: "chat"`. For fixed-policy Jev, remove `parent_model`,
`parent_protocol`, and `policy.parent`; do not use `--baseline`, which is a
separate deterministic first-tactic controller. Aliases are deployment-specific.

The shared encounter allows 90 seconds / 100 actions, requests parent guidance
at ten-action intervals (one request in flight), captures every fifth update,
and stops at two kills, health <=25, empty bullets, or model/runner handback.
Confidence threshold 0.1 is an exploratory setting, not a calibrated guarantee.
It supplies compact combat facts and a 31x31 ASCII collision map at 2m cells.
The model chooses bounded WASD, keyboard look, fire, use, or wait tactics; there
is no teleport, automatic aim, direct damage, or direct gameplay mutation.

## Live results

One corrected-observation run per mode, same reset/goal/menu; not a statistical
model comparison. All started at 100 health, 50 bullets, and zero kills. The
simulation continued during model calls and evidence capture.

| Mode | Actions / elapsed | Kills | Final health / bullets | Observed path | Handback |
| --- | --- | --- | --- | --- | --- |
| Fixed-policy Jev | 49 / 18.7s | 0 | 25 / 50 | 4.0m | Health cutoff |
| Luna + Jev | 41 / 18.7s | 0 | 25 / 70 | 36.1m | Health cutoff |
| Sol + Jev | 69 / 28.7s | 1 | 25 / 68 | 45.7m | Health cutoff |
| GLM + Jev | 50 / 19.2s | 1 | 55 / 47 | 14.0m | Jev unexpected-state handback |

Path is the sum of distances between observed positions, not an exact movement
integral. Ammo pickups explain increases. Fixed Jev oscillated between turns;
Luna guidance was followed by sustained movement through the room. Sol's kill
occurred **before its first parent update was installed**, so it cannot be
attributed to Sol's guidance. GLM's kill followed two applied guidance updates;
that chronology alone does not establish causality or superiority. No mode
consistently tracked and engaged the second enemy. GLM's final screenshot shows
the dead trooper, "Trooper down", 1/12 kills, and 55 health.

| Mode | Jev request median / p95 | Observation median / p95 | Gap between input calls median / p95 | Parent completions | Jev actions during those requests |
| --- | --- | --- | --- | --- | --- |
| Fixed | 164 / 324ms | 17 / 175ms | 194 / 448ms | — | — |
| Luna | 162 / 232ms | 15 / 272ms | 197 / 410ms | 6.69, 7.42s | 18, 15 |
| Sol | 164 / 247ms | 16 / 167ms | 186 / 366ms | 8.31, 7.45, 10.34s | 17, 15, 32 |
| GLM | 153 / 201ms | 15.5 / 185ms | 175 / 355ms | 5.82, 7.32s | 17, 18 |

These are client-observed request latencies, including router, network and
provider processing; they do not isolate model compute. Parent work did not
block Jev. Actual guidance cadence was limited by parent completion time, not
the requested ten updates. Input calls themselves lasted roughly 129–159ms at
the median, including the requested holds. Controls are released while Jev
thinks: this remains a roughly 2–3Hz controller, not continuous PID control.
Screenshots account for occasional observation spikes. Both model clients use
text only; original screenshots are retained as supervising-agent evidence.

## Problems found and corrected

- The local router served Responses SSE with a JSON content-type label. The
  client now recognizes the stream body; a regression test covers it.
- Initial Doom observations used Perception LOS, which did not include retained
  static meshes. Jev repeatedly fired at enemies behind a wall. Doom now uses
  the existing full Engine `CastSegment` for LOS and exposes `player.aimHit`
  from the exact same weapon-ray helper as firing. Shared Perception is tracked
  separately as Engine task **8385**; no Engine source or package change was
  needed for this experiment.
- Keyboard look and precision look make finite ordinary browser inputs useful
  for aiming; the observation reports rates, signs and target angular errors.

Earlier `fixed-final` / `luna-final` transcripts used the misleading LOS and are
retained as diagnostics, excluded from the comparison above. An earlier Luna
pilot also failed parent response parsing. The tested Doom checkout reports no
navigation projection; these runs used collision geometry, not a navigation
grid. This does not assert that navigation work in another checkout is absent.

## Follow-through

Next useful experiment: make the parent's target identity, destination and
engagement bounds more explicit, invalidate target-specific advice after a kill,
and reduce coarse-turn oscillation while keeping movement/aim decisions in Jev.
Use fresh angular errors rather than a stale numerical bearing in parent prose.
Compare that small change on the same encounter before attempting a full level.

Focused assistant race tests, the playtest package suite and `go vet` passed.
Doom's C# build and shell build passed. Evidence is under
`/home/dev/evidence/task8384/`: `*-encounter.json`, `*-encounter.jsonl`,
`*-encounter-result.json`, `metrics.json`, and original screenshots under
`playtest/browser/63578b62-e1f2-43db-b34a-d600a0b94ac5/`.

`warnings-final.json` completed both browser and Engine capture. Without a
compatible baseline it is report-only. It retained WebGL ReadPixels performance
warnings and one aborted `/__rusty/product/runtime/audio-feedback` request on
the separate capture attachment. The helper explicitly reports an unresolved
Error disposition; no clean warning, audio, or rendering claim is made. Original
controller captures contained no page errors. Earlier capture attempts failed
because of a browser-binary path mismatch and a continuous-page `networkidle`
wait; the final capture used the installed browser and DOM/canvas readiness.
