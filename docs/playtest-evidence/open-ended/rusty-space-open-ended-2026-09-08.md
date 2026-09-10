# Rusty Space open-ended native-controller evaluation

Model: gpt-5.6-luna configured worker; runtime model was not independently verified.

Repository/profile: `rusty-space` profile, remote URL `http://192.168.1.22:37301/`, local browser URL `http://localhost:37301/`.

Started: 2026-09-08T04:51:23Z for the final clean run. The supplied prepared lease was `a65b99bf-b643-435b-b8a9-ae4a552d531d`; it was stopped after an input-schema probe degraded it. A recovery left Firefox on its restore-session page, so that lease was stopped and a fresh run was started. The final gameplay lease was `97bc840a-bbef-46e2-9e7b-4dd6b1caaa58`.

Mission: use only native Xbox inputs to assess proportional thrust and steering at low/high values, simultaneous turn plus thrust, release, and Back reset behavior.

Outcome: `pass` as an operational evaluation run. The control observations are repeatable; reset was not evaluated because the attempted Back packet used an unsupported field and was silently ignored by the harness.

## Neutral observation

The page showed a dark top-down flight field with a cyan triangular ship near the center. The visible instructions read: “W thrusts. A and D steer. Mouse wheel zooms. R resets flight. F aborts. Xbox: RT thrusts proportionally, left stick steers, LB/RB steer, Back resets.” The page exposed `heading` and `speed` text. The camera kept the ship near screen center, so screenshots support heading/telemetry and ship pose but do not directly prove world translation.

The final run started at heading 73°, speed 7.4. Earlier fresh startup began at 73°, speed 6.9, and a low-thrust probe raised it to 7.4; starting a new browser did not reset flight state.

## Native input observations

All rows below are `playtest input` gamepad steps followed by `playtest observe`; each gamepad step was released by the controller before the observation.

| Input | Visible result | Evidence |
| --- | --- | --- |
| `lx: 0.25`, 1000 ms from 73°/7.4 | 87°/7.4; right turn about +14° | [`85959950-9ad1-4888-923d-0f105c46b2d4.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/85959950-9ad1-4888-923d-0f105c46b2d4.png) |
| `lx: -0.25`, 1000 ms from 87°/7.4 | 74°/7.4; left turn about -13° | [`d5e8788a-abc9-4f86-96a8-23d700a1032e.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/d5e8788a-abc9-4f86-96a8-23d700a1032e.png) |
| `lx: 1`, 1000 ms from 74°/7.4 | 194°/7.4; rapid right turn about +120° | [`620a87ad-ad9a-460c-a5e1-1dfba3e8cf1f.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/620a87ad-ad9a-460c-a5e1-1dfba3e8cf1f.png) |
| `lx: -1`, 1000 ms from 194°/7.4 | 72°/7.4; rapid left turn about -122° | [`16921884-d839-4a4c-b608-69f2a2260a8a.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/16921884-d839-4a4c-b608-69f2a2260a8a.png) |
| `lx: 0.1`, 1000 ms from 72°/7.4 | stayed 72°/7.4; no visible turn below the documented 15% deadzone | [`1be16da6-7734-4876-951f-507381c9acc4.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/1be16da6-7734-4876-951f-507381c9acc4.png) |
| `lx: 0.25`, 1000 ms from 72°/7.4 | 85°/7.4; repeat of the low positive turn | [`cb493686-df9d-4055-a297-186d31d985ee.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/cb493686-df9d-4055-a297-186d31d985ee.png) |
| `rt: 0.25`, 2000 ms from 85°/7.4 | 85°/7.8; gradual low-thrust increase | [`c6c40cc4-440b-4bc0-ba4a-d0562e2bf209.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/c6c40cc4-440b-4bc0-ba4a-d0562e2bf209.png) |
| `rt: 1`, 2000 ms from 85°/7.8 | 85°/11.0; much larger high-thrust increase | [`6a4a6c05-37de-4e40-9997-0f8071594ca3.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/6a4a6c05-37de-4e40-9997-0f8071594ca3.png) |
| `rt: 0.5`, 1000 ms from 85°/11.0 | 85°/12.0; reached the visible speed cap | [`7a1a6655-fae8-49f2-a5a5-f9443a95364c.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/7a1a6655-fae8-49f2-a5a5-f9443a95364c.png) |
| `rt: 1`, 1000 ms from 85°/12.0 | stayed 85°/12.0; high input is clamped at the cap | [`15acede9-15ad-4a70-95a2-a5218010ab3a.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/15acede9-15ad-4a70-95a2-a5218010ab3a.png) |
| simultaneous `lx: -0.5`, `rt: 0.5`, 1000 ms from 85°/12.0 | 36°/12.0; turn and thrust were accepted together, with the ship pose rotated accordingly | [`de677cc9-6783-4a68-a8f8-16e5fab083b0.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/de677cc9-6783-4a68-a8f8-16e5fab083b0.png) |
| neutral wait, 1500 ms after the combined hold | remained 36°/12.0 with the same pose; no stuck turn or thrust was visible | [`0ea55f23-f5c1-4e64-8aed-973119da2776.png`](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/0ea55f23-f5c1-4e64-8aed-973119da2776.png) |

## Findings

- Steering is controllable and consistent across low/high analog input. `lx: 0.1` is quiet, `±0.25` produces a small roughly symmetric turn, and `±1` produces a roughly symmetric rapid turn. The magnitude increase is consistent with an analog response after the 15% deadzone; the screenshots do not establish an exact transfer function.
- Thrust is visibly proportional over the tested range: `rt: 0.25` added about 0.4 speed units over 2 s, while `rt: 1` added about 3.2 over 2 s from a similar state. Speed then visibly capped at 12.0. Because the telemetry is rounded and the starting speeds differed, this supports proportional behavior but is not a calibrated physics measurement.
- Combined turn plus thrust is useful at the interaction level: both fields took effect in one native hold, changing heading while maintaining the ship at the speed cap. This particular combined probe cannot show additional thrust growth because the ship was already capped.
- Release behavior looked clean for steering. Neutral waits after steering and after the combined hold left heading stable, supporting that steering was not stuck on. Those observations were at or near the visible speed cap, so they cannot rule out a stuck-thrust condition from telemetry alone.
- Reset was not evaluated. The attempted packet was `{kind:"gamepad",back:true,ms:120}`, but the supported API requires `buttons:["back"]`; the harness silently ignored the unknown `back` field. The unchanged telemetry after that packet is therefore invalid reset evidence and says nothing about the game’s Back handling.

## Tool friction and cleanup

The supplied session accepted native gamepad RT/LX steps. The attempted Back packet used an unsupported `back` field that the harness silently ignored; it should have used `buttons:["back"]`, so that reset probe is invalid. A separate schema probe using a string keyboard key was rejected (`key must be an integer in [1, 255]`), and the rejected input degraded that session. Recovery created a new lease but left Firefox on a “Sorry, We’re having trouble getting your pages back” restore-session page. I stopped that lease, launched the documented `rusty-space` profile, and continued with validated gamepad fields. No game console, DOM injection, or product/service edits were used.

Final cleanup was verified at 2026-09-08T04:57:22Z: session `97bc840a-bbef-46e2-9e7b-4dd6b1caaa58` was `stopped`, its release had no errors, and its target reported `lease: null`, no lobbies, and no connected sessions. The complete final session artifact directory is `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/97bc840a-bbef-46e2-9e7b-4dd6b1caaa58/`.
