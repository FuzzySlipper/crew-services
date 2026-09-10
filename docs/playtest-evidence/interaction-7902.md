# Product interaction queries — #7902

## Delivered behavior

The installed crew-services service, CLI, MCP and supervised JavaScript worker
now expose `interaction`. Profiles explicitly opt in with
`interaction_queries: true`. The service reads the existing generated Engine
live-debug catalog from the game URL origin, then permits only
`interaction.query` or `interaction.cursor x y aspect`. It does not implement
another game-state registry, navigation, look assistance or activation path.

Every executed query retains its command, request/completion times, raw product
response, parsed facts and evidence path. JSON numbers retain their precision
in Go and persisted receipts. Product identity, incarnation, availability,
visibility and route outcomes are not reinterpreted. Cursor coordinates require
known normalized bottom-left x/y and a positive viewport aspect. Queries are
bounded and cancellable; unsupported profiles/catalogs fail explicitly. Normal
controller/keyboard input remains the only activation used here.

The opt-in example is `configs/playtest/interaction.example.json`; the operator
starts the demo separately and selects its reachable URL. Existing installed
profiles were preserved. The playtest skill and operator guide explain query
assistance and its limits.

## Actual packaged product and native run

Used the unchanged Engine `fixtures/csharp-controller-interaction` source in a
disposable copy, selecting published SDK/runtime pair
`0.1.0-dev.94490b482f96`. The copy ran on port 37303. No Engine bindings,
CraftSurvive source or existing demo endpoint were changed.

Installed Wolf/Gamescope/Firefox session:
`47fb28ba-f95b-4ad6-8e8c-942a174ba12c`.
Installed supervised script: `9389213c-274a-44bc-aa53-10b05a57e755`, completed
56 calls from `2026-09-09T03:26:19.697Z` to `03:26:27.875Z`.

The script read product queries between ordinary native inputs:

- Initially two chests were visible and `OutOfReach`, distance 5.056.
- Cursor `(0.768, 0.456)` at aspect `1280/720` reported the wall target
  `Occluded`; cursor `(0, 1)` reported `NoCandidate`. Neither changed look.
- Left-stick `ly:0.4` in 150 ms steps approached to z=-2.442, distance 2.665;
  both competing chests became `Ready`, with id 11 selected.
- RB cycled selected focus from id 11 to id 12.
- Corrected Xbox X opened id 12 through ordinary Engine input. `uses` became 1,
  id 12 advanced to revision 2 / `Unavailable`, and id 11 became selected.
- Keyboard K locked id 11, advancing it to revision 2 / `Locked`. Focus cleared.
  Another X left `uses:1` and reported `NoCandidate` for the use result.
  Old target observations therefore did not authorize the now-ineligible target.
- Right-stick look turned yaw to 2.827 radians; the reticle query returned
  `NoCandidate` and an empty candidate list.
- Every returned route remained `Unknown`; query responses explicitly reported
  `semanticTargeting:true` and `lookAssistance:false`.

Installed CLI interaction and MCP interaction calls also succeeded outside the
script. The MCP probe kept stdin open until its response; the first probe closed
stdin prematurely and correctly received cancellation. Both records are retained.

## Original visual evidence and receipts

Root inspected the original native screenshots. The initial frame showed two
tan chest-like blocks on a gray floor, a gray wall on the right, and Firefox's
pointer-control notification. The later frame showed the right chest blue and
the left chest tan, consistent with the product's displayed color legend. These
pixels support the visible change; locking and input admission are separate
product-query facts.

- Initial original:
  `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/47fb28ba-f95b-4ad6-8e8c-942a174ba12c/ca11287e-8bc5-4c0b-966c-3233385e0bad.png`
- Opened/locked original:
  `/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/47fb28ba-f95b-4ad6-8e8c-942a174ba12c/20f5327d-e28c-4e57-939b-b80dd9e46ffc.png`
- Capture sidecar:
  `/home/agent/.local/state/crew-playtest/capture-77c8cf09-e97a-4e17-9e00-24617a9adb0b.json`
- Complete script source, query/action results and checkpoints:
  `/home/agent/.local/state/crew-playtest/scripts/9389213c-274a-44bc-aa53-10b05a57e755/`
- CLI/MCP results, native result summary, launch/release and final status:
  `/home/agent/.local/state/crew-interaction-7902/`
- Xbox mapping correction and raw browser/device evidence:
  [xbox-mapping-7905.md](xbox-mapping-7905.md).

A separate browser-backend exercise also queried the same packaged C#
product, approached with keyboard input, opened a chest and rejected repeat use.
Its script `3c1e372e-9bad-4491-8c2d-fd87e5e3b8d5` and original captures are in
`/home/agent/.local/state/crew-interaction-7902/`. The first authored script used
unsupported `KEY_W` instead of documented `W`; it failed before delivering that
input and was corrected. Browser console records contain WebGL ReadPixels
performance warnings. This is not a GPU-performance or all-diagnostics-clean
claim.

## Checks and cleanup

`go test ./...`, focused session/client/CLI race tests, six Node worker protocol
tests and skill validation passed. Tests cover query opt-in and catalog absence,
fixed command/coordinate bounds, cancellation, large identity preservation,
durable query facts, and CLI/MCP/JS wiring. Independent source review found no
actionable defect.

Both product sessions were released. The temporary installed profile was removed
with byte-for-byte restoration of the prior games file; the disposable product
host and browser-only validation service were stopped. Installed
`crew-playtest.service` remains active with no session or target lease. The
remote target remains deployed with corrected X/Y mapping.

## Still unavailable: #7901 / Engine #7816

Engine #7816 remains planned. Current Engine controller-interaction documentation
explicitly leaves screenshot freshness to that task. Query observations are not
presentation revisions, and CameraQueries uses caller-supplied viewport facts.
Existing generic capture metadata remains useful, but Engine readiness and
remote frame correlation are still `unavailable` / not measured. #7901 remains
blocked only on that upstream work; this delivery does not fabricate readiness.
