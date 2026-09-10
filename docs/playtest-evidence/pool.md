# Two-slot pool acceptance — 2026-09-09 PDT

Deployed in crew-services against den-srv Wolf, with local pool capacity 2.
Config: `/home/system/crew-services/playtest/pool.json`.
See [operation and resizing](../playtest.md#configurable-tester-pool).

## Live acceptance

Space and CraftSurvive ran concurrently from their separate existing den-serve
URLs (`:37301` and `:37300`). No game source or demo server was changed.

| Check | Observed result |
| --- | --- |
| Concurrent allocation | Space `5b5e14cd-6c80-4c54-9c18-7cee6f0929f3` in slot-1; CraftSurvive `914ef232-2716-448b-91d8-642b169675a4` in slot-2 |
| Controller isolation | Space heading 0° after reset; still 0° after CraftSurvive's left-stick input; 35° after Space's own left-stick input |
| Script cancellation | Space script cancelled while CraftSurvive's 1800 ms look script completed and captured a frame |
| Queue and reuse | Third start queued with both slots occupied; stopping Space released slot-1 to new Space session `bff8e5d8-85e1-4f16-a457-4a81317134e7` |
| Peer survives reuse/stop | CraftSurvive captured after slot-1 replacement and after that replacement stopped |
| Recovery | CraftSurvive recovered into slot-2 with new session `9e01a11a-6976-4dd9-8377-62c4865771c2`, then captured successfully |
| Cleanup | Both slots free, no queued starts, all owned sessions stopped |

Original evidence: [Space reset](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/5b5e14cd-6c80-4c54-9c18-7cee6f0929f3/8e8b5b8f-aed6-4944-9b81-3f49149f0502.png),
[after peer input](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/5b5e14cd-6c80-4c54-9c18-7cee6f0929f3/91c2bca3-0e91-456c-a775-7e4db4d00dcb.png),
[after own input](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/5b5e14cd-6c80-4c54-9c18-7cee6f0929f3/db57488a-0807-45c2-bc46-d303cc868032.png),
[concurrent CraftSurvive](/home/dev/dsh-crew/experiments/wolf-den-srv/controller/state/slots/slot-2/914ef232-2716-448b-91d8-642b169675a4/ee719d0e-08d6-44a5-9bce-f0200a3262bb.png).
Receipts and capture sidecars are in [pool-2026-09-09](pool-2026-09-09/).

## Fixes established by live testing

- 20,000 Kbps stereo Moonlight configuration avoids the low-bitrate same-IP RTSP
  ambiguity documented in [Wolf PR #441](https://github.com/games-on-whales/wolf/pull/441).
  At 6,000 Kbps, simultaneous launch crashed Wolf; sequential launch lost the second stream.
- Exact controller event/js mounts replace the old whole-host input mount.
  Acquire clears only its own stale joystick records before the client connects;
  launch waits up to three seconds for a fresh controller record. Missing or
  ambiguous records fail before creating the browser lobby.
- Prepared Firefox profiles suppress terms/default-browser/session-restore prompts.
  `MOZ_LEGACY_PROFILES=1` prevents Firefox from silently creating an unprepared
  per-install profile. Pointer-lock notifications remain ordinary browser output.

Full `go test ./...` passed; race checks passed for pool, session, target and Wolf
adapter. After the freshness fix, target race checks and the full native sequence
passed again. Independent review approved the allocator and the final device
freshness boundary.

This is functional concurrency acceptance, not a performance benchmark or an
exhaustive multi-controller game certification. Both slots share GPU/CPU/network;
capture `pool_activity` is before/after occupancy metadata. Original frames retain
browser notifications and game output. Input receipts alone do not prove game
consumption; the heading comparison above supplies visible evidence for that check.
