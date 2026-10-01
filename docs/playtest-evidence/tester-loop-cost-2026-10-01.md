# Tester loop cost: browser vs engine lane, full vs compact answers (#8882)

Measured on 2026-10-01 with `scripts/measure-playtest-loop.py`, which drives
`playtest mcp` the way a model does. Mission: in held time, turn toward the
nearest bullets pickup and walk to it (`look` + `act forward` with `capture`)
until collected, then `act attack` with `capture`. All three runs collected the
pickup in 3 actions. The steering script reads its own full observations
outside the measurement, so the numbers cover the tester's action calls and
their answers.

| Lane (profile) | Answers | MCP calls / action | Text bytes / action | ≈ text tokens / action | Image tokens / action | Wall ms / action |
| --- | --- | --- | --- | --- | --- | --- |
| browser (`rusty-doom-study-hosted`) | full | 1.67 | 13,633 | 3,408 | 1,229 | 309 |
| engine (`rusty-doom-engine`) | full | 1.67 | 14,574 | 3,643 | 1,229 | 98 |
| engine (`rusty-doom-engine`) | compact (default) | 1.67 | 3,441 | 860 | 1,229 | 99 |

- **Calls:** observe → act → see the frame is one call; `look` adds one when
  the bearing changes.
- **Text:** compact answers (MCP's default) cut the text a model reads by about
  76%. The act's `delta`, receipts and step stay whole; the observation's
  lists and deep objects are summarized. The full result is kept at `receipt`.
- **Images:** one 1280×720 capture per action (≈1,229 tokens, estimated as
  w·h/750). The browser capture includes the HUD; the engine capture is the
  world frame named by its step.
- **Wall time:** the engine lane takes about a third of the browser's time per
  action: no page round trips or waits for a shown frame.

Tokens are estimated (text bytes / 4). The numbers are an observation, not a
gate. The earlier Luna trials' 120–300 CLI invocations per session are not
re-run here.
