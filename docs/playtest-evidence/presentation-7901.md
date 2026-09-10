# Engine presentation capture integration — #7901

Implemented and installed on 2026-09-09. `capture` accepts
`engine_presentation:true` (CLI/MCP/JS); profiles can default it with
`presentation_observations:true`. The service uses the existing generated debug
transport and fixed `engine.renderer.presentation` command. Its timed raw
response, parsed facts and query evidence path are attached to the image sidecar.
Query failure or unavailable data does not discard an otherwise usable capture.

The observation's `pending`/`submitted` state is separate from whole-world
readiness, GPU completion and remote frame correlation, which remain unavailable.
Comparison metadata uses submitted cameras, layout and actual CSS/backing
viewport; surface-local revision comparison requires equal runtime and surface.
Feedback for a different runtime binding is not considered comparable.

## Native installed validation

Used the unchanged Engine controller-interaction fixture in a disposable package
consumer, SDK/runtime pair `0.1.0-dev.cda0274a1f6d`, port 37303. Existing game
repos and deployments were not modified. Wolf/Gamescope/Firefox session:
`0519f00f-653e-45ff-8e4e-92767cb4a5f0`.

The parent explicitly invoked product `viewpoint.visit near`, `side`, then `near`
through the product debug transport, labelling the resulting captures as assisted
repositioning. Captures retained intermediate observations until the submitted
camera matched the requested pose. This checked a camera condition, not global
idleness or PNG frame identity. All intermediate sidecars remain available.

| Capture | ID | Engine state | Observation age | Submitted camera comparison |
| --- | --- | --- | --- | --- |
| Near | `018bacc3-bdf9-4746-8307-a8066ab06d9d` | pending | 138 ms | Initial reference |
| Side | `d0ccadf3-0159-4e3f-85c7-3cb88c68d889` | submitted | 608 ms | Different from near |
| Near return | `af4d88e8-3b67-4ffd-9b88-222feaad5786` | pending | 237 ms | Equal to near |

All three reported submitted CSS and backing dimensions of 1280×720. Near and
near-return reported configured camera `csharp-camera-1` at
`[0,1.5699999332427979,-2.5999999046325684]`, yaw 0°, pitch -8.764097213745117°.
Side reported `[-2,1.5699999332427979,-2]`, yaw 33.69007110595703°, pitch
-5.85915470123291°. Projection remained perspective, FOV 70°, near 0.05, far100.
The surface-local view revisions advanced even when the camera matched. Those
counters were not used as Rust publication revisions or screenshot identities.

Root inspected all three original native images. Both near images showed the
same large green left chest, tan center chest, partial right chest and slab edge
against the gray floor. The side image showed tan boxes from an oblique angle
and a tall gray slab at the right. The instruction overlay remained visible.
This is a visual composition observation, separate from semantic selection,
readiness and acceleration claims.

Original images:

- Near: `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/0519f00f-653e-45ff-8e4e-92767cb4a5f0/cf4af97b-e79c-4ef4-a657-2894752a4fb9.png`
- Side: `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/0519f00f-653e-45ff-8e4e-92767cb4a5f0/3c3116f8-741e-4722-b439-582b551fc3ea.png`
- Near return: `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/0519f00f-653e-45ff-8e4e-92767cb4a5f0/62d8eca8-1009-4ac9-8ef4-f518307e9a45.png`

Complete visit receipts, timed observations, capture sidecars, comparison summary,
validation program, installed JS result and MCP image/metadata response:
`/home/agent/.local/state/crew-presentation-7901/`.
Original query/capture artifacts also remain under
`/home/agent/.local/state/crew-playtest/`.

Installed supervised JS successfully called `capture` using the profile default.
Installed MCP returned its original image block and Engine metadata together.

## Checks, diagnostics and limits

Full Go suite passed. Focused capture/interaction/presentation and client race
checks passed. The existing large-journal stress test timed out during concurrent
full-suite/race work, then passed alone under race instrumentation; its timing
limit was not weakened. Skill validation and whitespace checks passed.
Tests cover missing/malformed observations without losing the image, raw response
retention, pending facts, stale binding rejection, surface replacement, override
behavior, durable metadata and revisions beyond JavaScript safe-integer range.

Engine's report-only `capture-playtest-warning-delta.mjs` ran around the native
exercise with Engine diagnostics capture complete, no lag/drop and no new events.
Its local browser stayed on about:blank; it did not observe the remote Firefox
console. No compatible baseline was supplied, so the report correctly does not
permit a global clean claim. See `warnings.json` in the validation directory.

GPU completion, whole-world readiness, exact PNG/Engine-frame correlation and
application adapter/hardware-acceleration identity remain unavailable. Submitted
backing dimensions and native GPU-session configuration do not establish all of
those properties. #7901 is complete for the published upstream contract; a future
capture handshake or visible frame marker would be separate Engine/harness work.

The temporary playtest session was released, its profile removed with the original
installed profiles restored, and the disposable product host stopped. The updated
crew-playtest service remains active. No source commits were created.
