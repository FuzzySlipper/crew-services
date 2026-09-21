# Bounded playtesting assistant

`playtest-assist` runs a short controller interval on an existing caller-owned
playtest session. Jev chooses a tactic; the ordinary playtest service executes
its fixed keyboard, mouse or controller batch. This is a debugging assistant,
not a general agent harness or a replacement for visual playtesting.

Build with `go build -o /tmp/playtest-assist ./cmd/playtest-assist`.
Start and visually inspect a session using `playtest`, then supply its ID in a
configuration based on `configs/playtest/assistant.example.json`:

```sh
playtest-assist --config /tmp/interval.json --output /tmp/interval.jsonl
playtest-assist --config /tmp/interval.json --output /tmp/baseline.jsonl --baseline
```

The session must be exclusively owned for the interval. The runner leaves it
open for planner inspection and requests input cleanup on every exit. Stop it
with `playtest stop SESSION` when finished. SIGINT/SIGTERM cancels the interval;
finite input batches still bound holds if the client is killed abruptly. An
uncertain input receipt is never retried automatically. Inspect the handback's
cleanup receipt; a cleanup error is not proof inputs were released.

## Observations and policy

The observer preserves original screenshot metadata and optional read-only
product observations. Jev is text-only: it cannot see the screenshot merely
because its path is present. `commands` may select the Engine spatial JSON map
or Doom's product readout. The URL must exactly match the session profile;
commands are checked against the live product catalog. These are semantic
assistance, not screenshot-derived facts. Spatial inspection is optional.
An empty command list permits capture-only observations, but a meaningful
controller needs suitable textual facts; do not imply visual perception.

Planner configuration supplies a goal, instructions, tactic descriptions and
fixed input steps. Each tactic is at most two seconds; total interval is at
most120 seconds. Jev cannot synthesize scripts, commands, durations or inputs.
The deterministic baseline repeats the first tactic using the same observation,
threshold, deadline and cleanup machinery.

`progress_pointers` are JSON pointers into the observation (including `facts`).
Unchanged selected facts for `stall_actions` consecutive actions cause handback.
Missing selected facts also cause handback. Select facts that measure useful
progress, such as player X/Z, rather than clocks, revisions or unrelated actors.
`stop_when` conditions use `eq`, `lte`, or `gte` and are evaluated independently;
any matching condition returns its supplied reason. Numeric comparisons require
numeric facts. Product-specific thresholds belong in this caller configuration.

The JSONL transcript records policy, timestamped observations, controller
inferences and raw probability responses, requested actions, input receipts,
latency and final handback. A model's `goal_complete` remains
`controller_goal_complete`; only an explicit observed threshold receives the
planner's threshold reason. Schema validity and confidence do not establish
correctness. Review original screenshots and product facts when interpreting it.

## Model route

Jev uses a typed decisions request, not chat completions. The default router is
`http://127.0.0.1:18082`, alias `jev`, with local `POST /v1/decisions` forwarding
to OpenRouter `POST /api/alpha/decisions`. `--router` and `--model` override these.
`PLAYTEST_ROUTER_TOKEN` supplies optional router authentication; the OpenRouter
provider key remains in den-router. No provider credentials enter transcripts.
The choice question includes permitted tactics and four handback choices:
completion, stalled, unexpected state, and uncertainty. Low confidence,
malformed responses and request timeouts return control without input.

## First live Doom interval (task8375)

Using the existing CoreCLR Doom session, both controllers approached the nearby
bullets with two 350 ms forward holds, then handed back when the observed Z
coordinate crossed -1.5. Original screenshots showed “Picked up Bullets”,
70 bullets, and ITEMS1/16. The model did not receive image pixels.

| Controller | Actions | Interval | Decision calls | Reported model cost |
| --- | ---: | ---: | --- | ---: |
| Fixed-forward baseline | 2 | 1.289s | none | none |
| Jev 1.13 through den-router/OpenRouter | 2 | 1.726s | 158 ms, 222 ms | $0.00071169 |

The two Jev choices were `forward`, with confidence 0.87 and 0.79. Full textual
observations consumed 5827 and11118 input tokens because the second decision
also included the previous snapshot. Keep maps small; larger intervals may
benefit from selecting a more compact observation. These are two calls, not a
latency benchmark or evidence of general gameplay competence. A fixed script
is already sufficient for this simple approach. The experiment establishes
ordinary-input continuation, real Jev decisions, bounded handback and useful
inspection evidence; branching or difficult navigation remains future usage.

Local original evidence and interval transcripts:
`/home/dev/evidence/task8375/`. Each transcript includes the full policy, captured
facts, original image paths, response probabilities and cleanup receipt. Final
browser/Engine warning capture is report-only without a compatible baseline.
The report retained two WebGL ReadPixels performance warnings and one aborted
`audio-feedback` request on the separate warning-capture browser attachment.
Both interactive intervals had no page errors. This attachment diagnostic is
not explained by the assistant changes and is not presented as a clean warning
delta; no rendering or audio correctness claim is made by this experiment.

## Concurrent parent feedback

A configuration may add `parent_model`, `parent_protocol` (`responses` or
`chat`), and `policy.parent`:

```json
{
  "parent_model": "codex-gpt-5.6-luna",
  "parent_protocol": "responses",
  "capture_every": 5,
  "policy": {
    "parent": {
      "every_actions": 10,
      "timeout_ms": 15000,
      "max_age_ms": 15000,
      "event_pointers": ["/facts/combat.observe/player/kills"]
    }
  }
}
```

This is a fragment to merge into a complete interval configuration. Without a
parent configuration, the fixed-policy Jev mode is unchanged. The parent starts
from the first observation and is asked again after the configured number of
actions, or when a selected event fact changes. There is at most one parent
request in flight. Jev continues on its current guidance while that request
runs. Completed guidance is installed at an action boundary; stale, invalid or
failed results are recorded without replacing current guidance.

Parent output describes the situation, objective, target, parameters and
preferred tactic IDs. These are semantic guidance for Jev, not generated input
scripts. The parent cannot change the caller's tactic menu, deadlines or stop
thresholds. Its `stop` request causes a labeled handback. The transcript records
guidance revisions, source observation time, parent latency, and how many
ordinary actions completed while the parent was thinking. The last 15 action
IDs are supplied as recent context.

`combat.observe` is Doom's compact read-only gameplay observation. It can be
combined with `spatial.map ascii` for a smaller local map than the JSON cell
array. Both are product/Engine observations, not browser-inferred game state.
`capture_every` reduces screenshot frequency; skipped captures are explicit,
not copies falsely labeled fresh. Screenshots remain original local evidence;
these model clients receive textual facts only. Keep the browser rendering.

`cycle_timing` separates observation work, Jev request latency, input-call
duration and gaps between input calls. `observation_age_at_input_ms` measures
wall time since that observation began; it does not establish the age of a
rendered frame or exact input-consumption tick. Controls remain finite batches:
concurrent parent updates do not imply continuous held input during Jev calls.

Parent routes are deployment-specific. On this machine `codex-gpt-5.6-luna` and
`codex-gpt-5.6-sol` use the Responses streaming endpoint; `glm-5.3` is a chat
route. Check availability before drawing model-quality conclusions. Parent
completion/failure and latency belong in the experiment report alongside kills,
damage, ammunition and route progress.

A complete starting configuration is
[`doom-parent-feedback.json`](../configs/playtest/doom-parent-feedback.json).
See [the task 8384 experiment](playtest-parent-feedback-experiment.md) for the
fixed-policy/Luna/Sol/GLM results, measured cadence, and current limitations.

For world-object interaction, observations may include `interaction.inspect`
and `interaction.help` from the Engine's shared `InteractionDebugModule`.
They list target identities, labels and current use/rejection facts. Explicit
`interaction.use <id> <revision>` is a mutating assisted action for the supervising
agent through `rusty-live-debug`, not an observation command or a hidden Jev
control. It invokes the ordinary product handler after fresh reach/visibility
checks. See the Engine's `docs/controller-interaction.md` green path before
resorting to repeated pixel hunting for containers or doors.
