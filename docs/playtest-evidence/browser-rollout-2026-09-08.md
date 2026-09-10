# Browser and evidence rollout, 2026-09-08

Implementation: crew-services #7899/#7900/#7901/#7903/#7904. #7902 remains
blocked on Engine #7817/#7898; current Engine does not expose the product-owned
interaction query contract. Engine frame/readiness correlation from #7816 is
also unavailable. No substitute gameplay/query state was introduced.

## Direct CLI/JS evidence

Validation root: `/home/agent/.local/state/crew-playtest-validation-20260908/`.
A browser-only service on loopback 48201 left the production Wolf service alone.
The supervised `todo.js` script used browser fill/press/click and two capture
calls through the same Go session service and CLI. Its original result is
`todo-result.json` and includes source/events paths and both original images.

Direct original-image inspection: before shows one unchecked item, “Inspect
shared session workflow”, and “1 item left”. After shows a checkmark and
strikethrough on that item, “0 items left”, and “Clear completed”. Both captures
preserve the full 1280×720 viewport and app UI. This passes the bounded web-form
and checkbox interaction check; it is not a general certification of TodoMVC.
The first integration run exposed missing PNG dimensions in adapter metadata;
that gap was sent back for correction before final deployment.

`space-capture.json` records a real Rusty Space page, not a test substitute. Its
original image visibly shows the ship and heading/speed UI. Canvas CSS dimensions
were 1280×720 while backing dimensions were 320×180. Renderer is explicitly
unknown; no GPU claim follows from either dimensions or screenshot. No game
controls or reset were used for this capture.

## Boundaries

Headless DOM operations are browser automation, not the Wolf native-input path.
DOM assistance is explicitly recorded and never treated as world-target
assistance. Wolf pointer lock remains unknown because its target has no browser
readback; target delivery and actual game consumption are separate facts.

The final judge inspects original images. Comparison receipts describe geometry
and caller-supplied viewpoint agreement; they do not provide image scores or
fabricated readiness. Product overlays remain preserved.

## Installed service acceptance

The final service uses a dedicated Playwright browser-binary directory, leaving
other tools' browser cache installations available. Existing Space, CraftSurvive
and Dagger profiles were preserved; `todomvc-browser` is an installed example.

An independent playtester used CLI browser operations (it did not choose JS):
added “Buy milk” and “Write playtest notes”, completed the first, checked Active
and Completed filters, cleared completed, and confirmed only the second remained.
One ambiguous `.toggle` selector was rejected normally; a specific row selector
worked. Original images are under
`/home/agent/.local/state/crew-playtest/browser/2d8bd0ab-8d34-48e6-8c4a-8301400f1fbd/`.
Root inspected originals `383e60da-3c0c-4e3e-9975-d9feca1824dc.png` (two items)
and `8aa4e313-78c6-4106-a51b-76b840120f42.png` (one remaining, Active selected).
Cleanup reported released/browser closed.

The installed `installed-check.py` scenario and `installed-*.json` records in
this validation root passed: CLI start and browser inspect, supervised JS
fill/press/click with paired captures, equal actual PNG dimensions, MCP browser
inspection and original image content, cancellation during a waiting browser
action, degraded session status, explicit recovery, empty isolated new context,
and stop. Both script and standalone browser actions now have durable JSONL
request/result records; original images remain separate immutable artifacts.

After browser sessions, the installed service launched `rusty-space` through
Wolf/Gamescope. Native left-stick input (0.4 for 200 ms) returned target-reported
one-step delivery; original images showed heading 52° before and 56° after.
The initial capture retained the Firefox pointer-lock notification and a
transient Moonlight slow-connection warning. The later image had neither.
This is a bounded input/backend-switch smoke test, not a stream performance or
frame-freshness certification. Wolf lock readback remains honestly unknown.
Release reported no errors and local capture stopped.

## Verification and remaining work

`go test ./...` passed. Focused race checks passed for browser, session, routing,
client, evidence and Wolf. Node script-worker tests and installer shell syntax
passed. Browser fixtures exercise disabled/no-target/obstructed candidates,
scroll geometry, same-looking replacement staleness, non-activating default
selection, HTTP failure and cancelled-operation cleanup. Durability tests cover
500 script calls, retained partial terminal history and failed state publication.

#7899, #7900, #7903 and #7904 are delivered. #7901's generic capture portion is
delivered, with Engine correlation still blocked on #7816. #7902 is blocked on
#7817/#7898. No Engine or game source changes were made in this work.
