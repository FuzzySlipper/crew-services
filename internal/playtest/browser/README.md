# Browser backend

This package owns one private, headless Chromium persistent context per playtest lease. Go starts a fixed Node executable with this directory's fixed `worker.mjs`; requests use JSON lines over stdio. It does not construct shell commands from a profile, URL, selector, or browser request.

The adapter uses the direct Playwright library. The current official Playwright CLI has durable named sessions and is useful as an agent-facing manual tool, but it owns process/session management and renders text output. The direct library gives the Go lease exact child ownership, bounded request cancellation, private artifact paths, structured DOM results, and a stable replacement seam for a future Windows transport. Playwright documents `chromium.launch()`/contexts for programmatic browser control and the CLI documents its own named persistent sessions. Sources consulted 2026-09-08: <https://playwright.dev/docs/api/class-playwright> and <https://playwright.dev/docs/getting-started-cli>.

`Config` requires a private state directory. It may name a Chromium executable; the local `/usr/bin/chromium` is suitable for this Linux adapter. `npm install` in this directory installs the Node library. Browser downloads are not required when that executable is supplied.

Browser requests are objects with one of these operations:

| `op` | Required fields | Result |
| --- | --- | --- |
| `inspect` | optional `selector` | URL/title, viewport, canvases, pointer-lock state, bounded DOM summaries, console/page errors |
| `click` | `selector` or `x`,`y` | normal Playwright mouse/locator click |
| `fill` | `selector`, `value` | normal locator fill |
| `press` | `key`, optional `selector` | normal keyboard/locator press |
| `near` | `x`, `y`, `max_distance` (1..256) | visible interactive candidate by viewport geometry and `elementFromPoint`, with a 30-second single-use token |
| `select` | `token`, optional `action` (`click` or `move`) | revalidates candidate identity, geometry and hit target before actual mouse movement/click |

`near` reports `no_candidate`, `ambiguous`, `disabled`, or `obstructed` without an action. A successful result has `outcome: "ok"` and a token. `select` returns `stale` when its one-use/expired token, geometry, identity, or hit target has changed. Assistance is DOM-only and is recorded as `near_cursor_dom`; it does not infer Engine world targets.

Native input accepts `hold`, `point` (absolute coordinate move), `click`, and `wait`; it rejects `gamepad` and relative `move` with `capability_unavailable`, because a page input API cannot provide native pointer-lock deltas. A normal session/script completion calls `Cancel`, which leaves an idle browser intact. Cancellation while an input/RPC is active kills its whole child process group, waits for exit, and marks the lease unavailable. The caller must stop or explicitly recover; no browser action is replayed or silently restarted.

`observe` saves an original PNG under the lease directory and reports image dimensions, viewport, canvas CSS/backing dimensions, actual `document.pointerLockElement` state, and bounded console/page errors. Renderer is `unknown`: this adapter does not create a new graphics context or claim a renderer it did not observe from the application.
