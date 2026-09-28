# Task 8692: live registry and pool reload

The implementation adds `playtest reload` over the existing loopback command
API. It validates the configured game and pool files before publishing either.
Sessions retain durable startup profiles; growth creates independent slots,
and shrink refuses occupied or reserved tail slots.

## Verification on den-agents, 2026-09-27

- Full `go test ./...` passed.
- Race tests passed for `cmd/playtest-service`, `cmd/playtest`,
  `internal/playtest/session` and `internal/playtest/pool`.
- `go vet` passed for the service command, pool and session packages.
- Unit tests cover malformed/unreadable files, invalid sizes and queue waits,
  duplicate profiles, failed slot construction rollback, reserved launches,
  queued starts waking on growth, idle shrink/regrowth, concurrent discovery,
  profile copy isolation and retained interaction/presentation query origins.

A temporary loopback service using the installed browser worker and local Vulkan
launcher exercised real Chromium sessions independently of the occupied shared
pool. It started A, changed A's registered URL, added B, and grew from one to
three slots. A still accepted input; B started in both new slots. Malformed game
and pool files and a shrink to one slot returned errors while all three remained
usable. B was stopped and started again after rejection. A later reload grew to
10 slots; ten concurrent browser sessions accepted input, and the newly created
sessions reported a non-fallback AMD WebGPU adapter. Stopping all owned sessions
allowed a successful shrink back to two. The test service was then stopped.

Evidence retained locally:

- `/home/agent/.local/state/crew-playtest-local/verification-8692/verification.json`
- `/home/agent/.local/state/crew-playtest-local/verification-8692/verify.py`
- `/home/agent/.local/state/crew-playtest-local/verification-8692/service.log`
- `/home/agent/.local/state/crew-playtest-local/verification-8692/original-session-after-reload.png`

The capture shows the original session's input result and its RX 9070 XT WebGL2
renderer plus non-fallback AMD RDNA 4 WebGPU adapter. This verifies reload and
session continuity, not ten heavy games maintaining a particular frame rate.

## Donor consultation

Adapted the validate-then-swap model from Den services
`mcp/internal/backend/locator.go` (`ReloadRoutesFromPath` and `ReloadRoutes`).
Exact local source was read. The playtest service keeps its own registry,
session snapshots and pool ownership; no donor product schema was imported.

## Deployment

Deployed on 2026-09-27 at 11:11 UTC after the user explicitly authorized ending
Daggerfall session `a5a86e2a-300b-44a0-973b-a4c30540f265`. Its stop receipt
confirmed release. No other session was active when the old service was stopped.
The staged CLI and service were installed; previous binaries are retained under
`/home/agent/.local/state/crew-playtest-local/verification-8692/pre-upgrade-binaries/`.

After the one-time binary upgrade, a local GPU check session was started at
capacity 2. Updating pool.json to size 10 and running `playtest reload` returned
capacity 10 with all five existing profiles retained. The original check session
still accepted a click, and the service PID remained 232177 across reload.
The owned check session was released; the final pool had ten free slots.

Deployment receipts:
`/home/agent/.local/state/crew-playtest-local/verification-8692/deployment.json`.

Installed service SHA-256:
`0895bc6639b4471fed778d82f03a522d16913de6fcb34f5355103ef8255e49bd`.
Installed CLI SHA-256:
`fb3e4b0ea099daf73717426d033de62e5a9e30c91c6a5a1fe7d7840d5609c104`.

The earlier 30-minute idle watcher timed out without stopping any sessions.
It was not rearmed. No commits were created; concurrent edits were preserved.
