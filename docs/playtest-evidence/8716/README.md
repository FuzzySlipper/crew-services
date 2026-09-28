# Forward-only Doom playtest implementation

Tasks: Engine #8718 / #8374 / #8719, crew-services #8716 / #8720,
Doom #8721. Tested on the local GPU host on 2026-09-28 UTC.

## Result

Engine owns realtime/manual/action-driven simulation admission, independent
continuous/on-demand drawing, held presentation time, and detached observation.
The existing host scheduler and fixed updates remain authoritative. C# exposes
live action timing and product facts. The ordinary installed `playtest assist`
CLI/MCP composes input, bounded advancement, release, observation and captures.

The source is uncommitted. Doom uses local SDK
`0.1.0-dev.playtest-20260928a` and companion runtime
`.runtime/playtest-development-20260928g`; these are development artifacts,
not a published release pair. The final normal launcher started successfully
with live debug on port 4394. The installed playtest service is on port 48200.

## Practical observations

- An independent playtester defeated two troopers and collected shells and a
  shotgun. Current weapon timing changed from pistol 542.86 ms to shotgun
  1257.14 ms. Ordinary attack taps retain a single attack/recovery window.
- Parent continuation approached, opened and crossed the north door with normal
  keyboard controls. `door-used.json/png` shows opening; `door-crossed.json/png`
  shows raised 2.5, open, and player x=10.64 beyond the doorway.
- Static navigation returned `NoPath`; a floor lip blocked walking. Ordinary
  W+Space in realtime crossed the lip, followed by action-driven movement and E.
  This is a known navigation limitation, not an automatic route success.
  Route guidance now reports `route-unavailable`, retaining the eventual door
  action with `eligibleNow:false` until ordinary focus permits use.
- Final installed run moved about 1.18 units with a 200 ms action. In on-demand
  mode render sequence changed only 511 -> 512 for the next explicit frame;
  the action did not issue routine draws. Continuous mode continued submitting
  frames while time was held. GPU resources remained resident.
- Held look changed the camera to 90 degrees immediately. Four/eight view
  surveys returned `restored:true`; observer views did not move the player.
- `held-effect-capture.png` and `held-effect-after-capture.png` show the same
  pistol muzzle flash while looking around. Cooldown remained 426.19 ms and
  bullets remained 49 in the paired observations; inspection did not age it.
- `final-shot-record/` contains an armed-before-action pistol recording with
  original PNGs, MP4, GIF and contact sheet. `realtime-record/` proves a short
  wall-time recording. Both consumed one bullet. Actual sampling is slower than
  nominal FPS; no claim that every rendered frame was captured.
- `record-attack/` retains the independent playtester's shotgun clip.

Copied manifests preserve original source paths and timestamps. Files also exist
beside their copied manifests here with the same basenames. Reading captured
frames does not rewind live gameplay.

## Checks and review

Passed focused Rust lifecycle, product host and C# runtime tests; browser-host,
renderer-host and Three renderer suites; Go CLI/client/browser tests; and the
three harness action tests (fresh timing, tap/held release, uncertain delivery).
Doom build, lifecycle/recipe exercises, boundary audit and catalog checks passed.
The independent product reviewer also ran the full C# spine including CoreCLR,
NativeAOT and staging. Final route-guidance change rebuilt successfully.
Logs included here retain check output.

Doom's required Engine-ownership, trust-boundary and product review lanes all
finished without outstanding findings. Product review initially found missing
ammo refusal and absent defeated enemies in compact telemetry; both were fixed
and rereviewed. The final route eligibility delta was also rereviewed.

## Limits and use

No rewind, seeded encounter script or Jev dependency was added. Shared host game
state/time remains shared; per-browser observer/drawing settings remain local.
Restart the owned product for a fresh world. Close/recover cancellation uses the
existing browser-session path; ordinary failures release keys and return partial
results without retrying the action. Dagger #8722 is deferred.

See ../../playtest.md for commands and the Engine/Doom playtest-inspection docs
for ownership and development artifact setup.
