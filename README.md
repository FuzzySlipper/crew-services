# crew-services

For game and browser testing, see [Agent playtesting](docs/playtest.md),
[tester prompts](docs/playtest-agent-prompts.md), and the
[Engine product integration guide](docs/playtest-product-integration.md).
`playtest-service` owns sessions, their product hosts and input; `playtest`
provides CLI/MCP access and a supervised JavaScript worker composes actions.
[den-serve](docs/den-serve.md) runs local dev/demo servers for people and hosts
for playtest sessions. These local tools do not depend on the messaging fabric;
see [local-machine services](docs/local-services.md) for what runs on the agent
box and how it came from den-services.

`crew-services` is an independent, runtime-neutral successor for selected local
agent-service capabilities. Its implemented foundation includes a runtime-neutral
directory, atomic immutable message acceptance, an ordered delivery ledger, and
an adapter-owned session projection with an append-only event log. It stores
accepted work and read-model facts only; runtime activation and native insertion
remain adapter-owned and are not implemented here.

Rusty Crew, DSH, the Codex app server, and future transports are evidence and
adapter targets, not the core contract. The current implementation is one
boring local Go binary with SQLite and loopback JSON/HTTP.

The fabric records durable delivery, the sender's activation policy
(`next_turn`, `next_step` or `no_wake`; none of them interrupts),
claim/dispatch/reconciliation state, and optional reply rounds; runtime drivers
decide how to deliver into, resume, or activate their native agents. Drivers
register each session they drive with `POST /v1/session-addresses` and get a
stable `s/…` address for it. The session projection and event log are frozen:
still readable, but no longer extended. The direction is in
[docs/messaging-redesign.md](docs/messaging-redesign.md).

## Run locally

From this repository, start the trusted-box foundation with an explicit
database path:

```sh
go run ./cmd/crew-messaging -db ./crew-messaging.db
```

It listens on `127.0.0.1:8787` by default. `GET /healthz` and `GET /readyz`
return the local database readiness status. Use `-listen`, `-lease-duration`,
and `-ttl` to set active loopback and message-lifetime configuration.
`-retention` is currently parsed and validated but reserved and non-actuating:
no retention worker consumes it yet.

Alongside health/readiness, the local JSON boundary exposes adapter registration
and renewal; create/CAS-update/unbind, resolve, and list address operations; and
message submission/inspection plus delivery/mailbox inspection and cancellation
under `/v1`. Submission atomically records one immutable envelope and one queued
delivery, with producer-scoped `operation_id` retries. Inspection is read-only.
Claims, dispatch acknowledgement/reconciliation, reply rounds, and explicit
maintenance reaping are available. Native activation remains adapter-owned.
The session surface adds idempotent adapter adoption, revision-fenced updates,
bounded session/event reads, and SSE replay from a durable global cursor.
Opaque adapter keys and lease tokens never appear in session/client JSON.

Start here:

- [Rusty Crew messaging survey](docs/rusty-crew-messaging-survey.md) records
  the useful behavior and failure lessons from the donor.
- [Messaging service specification](docs/messaging-service-spec.md) defines
  the proposed fabric boundary, v1 contract, and proving slice.
- [Agent-box runbook](docs/agent-box-runbook.md) gives the loopback binary,
  SQLite, user-service, DSH adapter, and focused proving path.

The project deliberately does not promise Rusty Crew compatibility, remote
deployment, federation, authentication frameworks, or a UI. Those are not
implementation omissions; they are outside the first slice.

## Claude Code driver

`crew-claude` delivers fabric messages into Claude Code sessions through each
session's own inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`). It never starts,
stops or interrupts a session: a `next_step` message is read between tool calls
of the active turn, and anything else waits for the turn to end or starts a turn
in an idle session. Install it with:

```bash
scripts/install-crew-claude.sh
```

That builds `~/.local/bin/crew-claude` and starts `crew-claude.service`. It also
adds four things to `~/.claude/settings.json`, after backing it up:
- a SessionStart hook that registers the session and tells it its crew address;
- UserPromptSubmit and Stop hooks that report busy and idle;
- `crossSessionInbound: accept`.

Each hook makes one loopback call with a short timeout and always exits 0, so a
stopped driver doesn't break sessions. `--service-only` skips the settings
change. `--fill-notify` adds a PreToolUse hook that fills `notify_address` on
`submit_task_for_review`; enable it only once Den accepts that field. Undo it all
with `--uninstall`. Sessions started before installation get no address until
they restart.

## Codex-backed review runner

`crew-review` is the first managed review runtime. It admits durable review
jobs locally, asks Den MCP for the current bounded reviewer context, starts
private ephemeral Codex App Server threads with
`/home/system/crew-services/reviewer.md`, and sends only the structured
`complete_review` result back through Den's `finalize_review` tool. Den remains
the authority for current rounds, finalization, task transitions, and receipts;
the local SQLite job state is only the retry/recovery ledger.

Run it on the trusted agent box with the same user's Codex executable and
profile environment:

```sh
go run ./cmd/crew-review \
  -listen 127.0.0.1:8413 \
  -db "$HOME/.local/state/crew-review/crew-review.sqlite" \
  -den-mcp-url "${DEN_MCP_URL:-http://192.168.1.5:5199/mcp}" \
  -review-profile "${CREW_REVIEW_PROFILE:-/home/system/crew-services/reviewer.md}" \
  -codex-model "${CREW_REVIEW_MODEL:-}" \
  -codex-effort "${CREW_REVIEW_REASONING_EFFORT:-}" \
  -capacity 2
```

The installed service keeps ordinary machine configuration in
`/home/system/crew-services/crew-review.env` and its dedicated reviewer
instructions beside it in `reviewer.md`. `CREW_REVIEW_LISTEN`,
`CREW_REVIEW_DB`, `CREW_REVIEW_MODEL`, `CREW_REVIEW_REASONING_EFFORT`,
`DEN_MCP_TOKEN`, `CREW_REVIEW_PROFILE`, `CREW_REVIEW_CAPACITY`,
`CREW_REVIEW_RUN_INTERVAL`, `CREW_REVIEW_SUBMISSION_INTERVAL`,
`CREW_REVIEW_SOURCE_GRACE`, and `CODEX_COMMAND` may be supplied through the
environment; all have corresponding flags where they affect the process.
The command starts a fixed number of bounded runner lanes (one durable job per
lane at a time), reports `backend: "codex"` from `GET /v1/review-pool`, and
never exposes ephemeral Codex worker or thread IDs in that projection.
The pool projection includes bounded active jobs and a separate `finalizing`
count so durable reconciliation cannot hide behind the running aggregate. The
Den adapter validates the exact encoded 16 KiB finalization request before it
is stored; a rejected tool result lets the reviewer submit a shorter completion
in the same turn. Deterministic Den validation failures become one terminal
job failure, while ambiguous transport failures retain the exact request for
idempotent retry.

A repository can have a post-review hook: `-looks-good-hook
owner/name=/absolute/executable` (repeatable, or `CREW_REVIEW_LOOKS_GOOD_HOOKS`
with entries separated by `;`). After Den stores a `looks_good` receipt for a
managed submission of that repository, crew-review runs the executable once,
one hook at a time, within `-hook-timeout` (45 minutes by default). It passes
`CREW_REVIEW_REPOSITORY`, `CREW_REVIEW_COMMIT`, `CREW_REVIEW_REF`,
`CREW_REVIEW_PROJECT`, `CREW_REVIEW_TASK` and `CREW_REVIEW_ROUND` in the
environment, writes the output to `hooks/` beside the database, and posts the
outcome with the output's tail on the task thread. A hook never changes the job
or its Den round, and manual best-effort reviews, which have no commit, run no
hook. The installed service uses this to deploy reviewed den-services commits.

An operator may deliberately retry one terminal failed job with `POST
/v1/review-jobs/{id}/retry`. The action preserves the exact durable job and
Den round, releases any idle retained worker for that task before requeueing,
and clears the failed attempt's finalization, receipt, and failure detail. It
rejects missing, nonfailed, or busy-affinity jobs rather than replaying an
admission or retrying automatically.

`POST /v1/review-pool/check` probes the reviewer runtime on request; nothing
runs it on a schedule. With the Codex backend it runs `codex sandbox -c
sandbox_mode="read-only" git rev-parse HEAD` in the most recent review's
checkout (or `true` in the home directory before any review), the same
read-only sandbox reviewer turns use, and returns `ok`, the command, and its
output. A broken sandbox fails every reviewer command while turns still
complete, so reviewers end without `complete_review` or judge from the handoff
alone; this check shows it directly. When a turn ends without
`complete_review`, the job failure carries the turn's status, Codex's error,
and the reviewer's last message. A Den finalization conflict means another
reviewer finalized the round first, so the job ends `stale`.

The managed submission boundary is `POST /v1/review-submissions`. The Den MCP
facade routes its `submit_task_for_review` green path to this endpoint through
the separately configured `crew-review` backend. A first call records the Den
round and the task commit's GitHub check gate and returns at once, usually with
`phase: "gate_pending"`. One call is enough: a background pass (every
`-submission-interval`, 30 seconds by default) keeps advancing unfinished
submissions through gate waits and Den unavailability until `phase:
"job_admitted"`, including across restarts. Den may satisfy the gate with
checks from a later commit of the ref that contains the task commit.

Before admitting a reviewer, crew-review checks that the resolved checkout's
repository holds the submitted commit (`git cat-file -e`), so the reviewer can
read it with git. The working tree may be at another commit: agents often push
from per-task worktrees, whose commits share the checkout's object store, and
nothing pulls the main checkout. When the commit is absent crew-review runs one
`git fetch`, which updates objects and remote-tracking refs but never the
working tree, HEAD, or local branches. If the commit is still missing the
submission waits quietly in `source_pending`; after `-source-grace` (15 minutes
by default) it stops as `source_missing` with `error_code:
"checkout_missing_commit"`. This never produces a review verdict or a message
to the submitter. The admitted job carries the submitted
`base_commit..commit_sha` range; the reviewer prompt names it, says to read
files at the reviewed commit with `git show <sha>:<path>`, and warns that the
working tree may not match.

Repeating the same submission is an idempotent replay. While no reviewer job
exists, a repeat with a corrected `review_summary_md` or `reviewer` revises the
submission (and records a new Den round for the new summary) instead of
conflicting. A repeat of a `gate_failed` submission asks Den to re-evaluate the
gate, for example after a GitHub re-run; a repeat of `source_missing` re-checks
the checkout. The background pass never re-checks those terminal outcomes on
its own. Once a submission's reviewer has returned `changes_requested`,
submitting the same target again (or using the manual action) starts a fresh
submission and Den round, so a finding answered without a new commit can still
be re-reviewed; after `looks_good` a repeat only replays. Submission state and
round-scoped job admission are durable in the
local SQLite file, so an uncertain retry reconciles instead of starting a
second job. An unavailable crew-review backend is returned as an actionable
retryable result; there is no automatic Rusty fallback.

Den Web's contextual manual action uses
`GET /v1/projects/{project_id}/tasks/{task_id}/manual-review` to read a
server-evaluated capability and `POST` to the same path to admit it. The
service resumes an exact submission only when its durable source record
matches Den's current review round. Otherwise it admits an explicitly
commitless best-effort review of current `main`, with the canonical task
description and bounded task messages included in the reviewer context. Both
paths recheck that the task is still in `review`, and POST retries converge
through `Idempotency-Key`.

## Codex driver

`crew-codex` delivers fabric messages into threads of the shared Codex
app-server daemon, the one the TUI attaches to
(`~/.codex/app-server-control/app-server-control.sock`). It connects to that
socket (WebSocket, one JSON-RPC message per frame, which is what
`codex app-server proxy` forwards) and never starts, stops or restarts the
daemon or a thread. If the daemon is down it retries every poll interval.

Every thread the daemon has loaded gets a session address (`s/...`) through
`/v1/session-addresses`. The driver finds threads through `thread/loaded/list`
on each pass and the `thread/started` notification. It skips ephemeral threads
and threads that refuse direct input (subagents). It remembers the threads in
`-state` so mail can still wake a thread after the daemon unloads it, and
forgets threads not loaded for 14 days. Their mail waits in the fabric until
the thread is loaded again. Repeated `-address NAME=THREAD_ID` flags pin
named addresses as well. A named address already bound to `crew-codex` keeps
its target and generation, so queued work survives the switch.

Each binding advertises `deliver_next_step`, `deliver_when_idle`,
`durable_next_turn` and `wake_inactive`. Busy and idle come from the thread's
status (`active`, `idle`, `notLoaded`; `systemError` holds the work):

| Fabric action | Codex call |
| --- | --- |
| `deliver_next_step` (a `next_step` or `no_wake` message, thread active) | `turn/steer` with the active turn id. Codex records it as a user message at once; the model reads it before its next request. A steer Codex refuses because the turn ended falls back to `thread/queue/add`. |
| `deliver_at_idle`, `register_next_turn` | `thread/queue/add`. Codex starts queued items FIFO once the thread is idle. |
| `wake_then_deliver` (thread not loaded) | `thread/queue/add`, then `thread/resume` so the daemon loads it and starts the item. |

The `clientUserMessageId` is `crew-delivery:<delivery id>`. Codex keeps it on
the user message (and on the queued item), so when a native response is lost
the driver looks for it in `thread/queue/list` and the last turns
(`thread/turns/list`) and acknowledges or records `outcome_unknown`. It never
sends the message twice. A dispatch left in flight by a crash is settled the
same way on the next pass. Messages arrive as user messages that start with
`Crew message from <sender>`.

Install, or update the binary and unit (pins of an existing unit are kept):

```bash
scripts/install-crew-codex.sh --service-only   # binary and crew-codex.service
scripts/install-crew-codex.sh --hooks-only     # SessionStart hook in ~/.codex/hooks.json
```

The SessionStart hook (`crew-codex hook session-start`) registers the thread
through the driver's loopback API (`-control-listen`, default
`127.0.0.1:8788`) and tells the agent its crew address as additional context.
Codex asks to trust new hooks before it runs them. `--fill-notify` also adds a
PreToolUse hook that fills `notify_address` on `submit_task_for_review`; Codex
0.163 applies `updatedInput` (with `permissionDecision: allow`) to MCP tool
calls. Enable it only once Den accepts that field.

Known limits:
- The queue API is experimental. It holds at most 100 items per thread and
  lives on disk in `~/.codex/queue_1.sqlite`. Queued items have not been seen
  surviving a daemon restart yet.
- A thread `crew-codex` resumes has `crew-codex` as its only subscriber until
  a client attaches. An approval request in that turn waits unanswered until
  someone opens the thread.
- A steer never cuts short a running tool call. Checked twice with a
  `sleep 20` shell command: the steered note was recorded in the active turn
  at once and the command ran its full 20 seconds. Whether the model acts on
  the note is up to the model: once its reply followed the note, once (with
  the `Crew message from` framing) it did not.
