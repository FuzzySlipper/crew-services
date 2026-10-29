# Messaging redesign: durable core, thin drivers

Status: draft for discussion, 2026-10-28. Nothing here is implemented, and it
does not yet change `AGENTS.md` or the [messaging service
specification](messaging-service-spec.md). Where the two disagree, the spec is
still what the code does.

## Why now

The fabric has been idle since 2026-08-27. `crew-messaging` and `crew-codex`
still run, but the database holds 51 messages in total, and the only recent
writes are adapter lease renewals. Most of the earlier effort went into owning
and mirroring runtimes, not into delivering messages.

Since then, all three runtimes we care about have shipped a native way for
another process to put a message into a session. An idle session wakes up, and
a busy one sees the message at its next step without being interrupted:

| Runtime | Busy: seen at the next step | Idle: new turn | Native receipt |
| --- | --- | --- | --- |
| Claude Code (2.1.310) | per-session inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`), read between tool calls | same socket | accepted on the socket; held in memory only (about 50 queued; held messages expire after about 5 minutes) |
| Codex (0.163) | app-server `turn/steer` (needs `expectedTurnId`; drained before the next model request) | `thread/queue/add` (FIFO; the app-server starts it once the thread is idle) | `queuedSubmission.id` / steer `turnId`; `clientUserMessageId` is carried through |
| DSH (upstream 0.2.2-alpha.2) | web host `session/prompt` `mode:'steer'` | `session/prompt` `mode:'queue'`; resumes a cold session | `accepted:true`; idempotent on `requestId` |

None of them is durable across restarts, has delivery receipts we can reconcile
later, or deduplicates beyond a short window. No protocol standard covers
pushing an unsolicited event into a local agent session either. MCP Tasks only
cover work the agent itself started, and MCP Events, A2A push and ACP v2 are
still drafts, webhooks only, or editor-to-agent. So durable delivery is still
ours to own. Activation is now a small piece of code per runtime.

## What changes

1. **Drivers stop owning runtimes.** A driver posts into the runtime's own
   inbox and never supervises the runtime process. In particular, `crew-codex`
   stops spawning its own `codex app-server --stdio` and connects to the shared
   Codex daemon (`~/.codex/app-server-control/app-server-control.sock`) that
   the TUI already uses. Today it runs a second app-server against threads the
   daemon may also have loaded.
2. **Steering is not interrupting.** The old rule treated "insert into an
   active turn" as unsafe. Every runtime now defines a steer as "seen at the
   next step, never cancels", which is the behavior we want for urgent notes.
   The activation policy becomes a delivery choice made by whoever sends the
   message (see below). Interrupting remains out of scope.
3. **Sessions register themselves.** A SessionStart hook or plugin in each
   runtime binds the session to an address. The adoption and projection
   machinery is not needed for that.
4. **The transcript mirror goes.** The session projection, event log and SSE
   replay (1,619 events, the last from 2026-09-04) copy runtime state the
   runtimes already keep. Nothing outside this repo reads them. Retire them,
   or at least freeze them, unless a consumer turns up.
5. **Producers send to an address.** A service that learns something an agent
   should know, starting with crew-review learning that a gate failed, submits
   one fabric message to the address it was given. It does not need to know
   which runtime is listening.

## What stays

The core contract in [messaging-service-spec.md](messaging-service-spec.md)
holds up and stays as it is:

- immutable envelopes with producer-scoped `operation_id` idempotency
- bindings with semantic generations, so a rebind fails old work with a reason
  instead of misrouting it
- the delivery state machine (`queued → claimed → dispatching → delivered`,
  with `outcome_unknown` for ambiguous native outcomes)
- TTL, FIFO order and reason codes
- inspection that never claims or wakes anything

Rounds stay as they are. They are unused so far and are not part of the first
slice.

## Activation policy

The spec's `activation_policy` (`wake_when_idle` | `never_wake`) becomes:

| Policy | Target busy | Target idle | Target not running |
| --- | --- | --- | --- |
| `next_turn` (default) | held until the current turn ends, then delivered as the next turn | delivered as a new turn | stays queued until the session starts again |
| `next_step` | seen at the next step boundary; the turn keeps going | delivered as a new turn | stays queued until the session starts again |
| `no_wake` | seen at the next step boundary | stays queued until the session is next active | stays queued |

The sender picks the policy. Agent-to-agent conversation defaults to
`next_turn`; CI and review gate notices use `next_step`. A driver reports which
policies it can deliver natively. When it cannot honor one (for example, a
runtime with no idle signal asked for `next_turn` while busy), the delivery
stays `queued` rather than being delivered early.

## Addresses

A session address is created when a session first registers and is reused when
the same native session resumes. Adoption is idempotent on
`(driver, native session id)`, as session adoption already is. The address
itself is fabric-generated (for example `s/7f3a…`) and does not contain the
native id, so the core stays runtime-neutral. A rebind to a different native
session advances the generation as it does today.

Named role addresses (for example `rusty-engine/implementer`) stay possible but
are not needed for the first slice.

The agent learns its own address from the SessionStart hook's context ("your
crew address is …"). Tools that accept a notify address get it from there.
Where a runtime lets a PreToolUse hook rewrite tool input, the hook fills it in
automatically.

## Drivers

Each driver is a separate command in this repository. Core packages never
import a driver.

**Claude Code (`crew-claude`, built in #10452).**
- *Inbox protocol (Claude Code 2.1.310, read from the client and checked live):*
  - Connect to `CLAUDE_CODE_MESSAGING_SOCKET`.
  - Optionally send `{"type":"auth","token":…}` first.
  - Then write one JSON line per message: `{"msgV":1,"msg_id":"<uuid>","type":"user","message":{"role":"user","content":"<text>"},"priority":"next"|"later"}`.
  - The session answers nothing on the connection. Held, denied and expired messages are reported only as `peer_message_status` frames to a sender's own inbox, which the driver doesn't have.
  - `next` is read between tool calls of an active turn, or starts a turn when idle. `later` is held until the current turn ends. `now` preempts and is never used.
- *Registration:* a user-level SessionStart hook (`crew-claude hook session-start`) sends the session id, socket and token to the driver and puts the session's address into context. The socket and token change on every process restart, so they stay in the driver's 0600 state file (`~/.local/state/crew-claude/sessions.json`), not in the fabric binding.
- *Busy or idle:* UserPromptSubmit marks the session busy and Stop marks it idle. A session with no socket file is not running.
- *Delivery:* `deliver_next_step` goes out with priority `next`. Every other delivery goes out with `later`, so stale busy/idle state can make a message late but never early.
  - The driver dials before it records the dispatch, so a session that is gone sends the claim back to the queue.
  - A write that fails after the dispatch is recorded becomes `outcome_unknown` and is never retried blindly.
- *Inbound control:* the installer sets `crossSessionInbound: accept`. Without it, a session that bypasses permission prompts holds the driver's messages for approval.
- *Loss window:* the session's queue is in memory, so a message accepted just before the process exits can be lost. `delivered` means the socket accepted it, not that it was read.
- The `asyncRewake` fallback was not needed.

**Codex (`crew-codex`, reworked in #10453).**
- *Connection:* the daemon's control socket speaks WebSocket (one JSON-RPC message per text frame; `codex app-server proxy` only forwards bytes), so the driver dials it directly. It never starts, stops or restarts the daemon or a thread, and never uses the `/daemon/shutdown` path.
- *Registration:* every loaded thread gets an `s/...` session address, found through `thread/loaded/list` each pass and the `thread/started` notification, which reaches every connection. Ephemeral threads and threads with `canAcceptDirectInput: false` are skipped. A SessionStart hook (`crew-codex hook session-start`) registers the thread too and returns its address as additional context. Named `-address` pins stay; one already bound to `crew-codex` keeps its target and generation.
- *Busy or idle:* `thread/read` status each pass (`active`, `idle`, `notLoaded`; `systemError` holds the work). `thread/status/changed` also reaches unsubscribed connections and triggers a pass.
- *Delivery:*
  - `deliver_next_step`: `turn/steer` with the active turn id from `thread/turns/list`. Codex refuses a steer with no active turn (`-32600 no active turn to steer`); that falls back to `thread/queue/add`. An ambiguous steer never falls back.
  - `deliver_at_idle` and `register_next_turn`: `thread/queue/add`; the item leaves the queue and starts as soon as the thread is idle.
  - `wake_then_deliver` (`notLoaded`): queue, then `thread/resume`.
- *Reconcile:* `clientUserMessageId` is `crew-delivery:<delivery id>`. Codex keeps it as the user message's `clientId` for both steered and queued input, so a lost response is reconciled through `thread/queue/list` and the last turns. A dispatch left in flight by a crash shows up as the claim's `head_dispatching` head and is settled the same way.
- *Removed:* the private `codex app-server --stdio` child, the session projection writes, `thread/read includeTurns`, the control API (create thread, approvals and input requests, interrupt) and the `crew_directory`/`crew_message` dynamic tools. Those depended on owning the connection that started a thread; a TUI thread's approvals belong to the TUI. crew-review's ephemeral reviewer workers still use their own stdio app-server.
- *Findings (codex-cli 0.163.0):* a steer during a `sleep 20` shell command was recorded in the active turn at once and did not cut the command short. In one run the reply followed the note; in the fabric live check (with the `Crew message from` framing) the reply ignored it, so a steer is seen, not necessarily obeyed. Queued items are stored on disk (`~/.codex/queue_1.sqlite`), but surviving a daemon restart was not tested, since restarting the shared daemon would disturb the owner's sessions. A Codex PreToolUse hook can rewrite MCP tool input (`updatedInput` with `permissionDecision: allow`), so `notify_address` can be filled the same way as in Claude Code.

**DSH (`crew-dsh`, new; needs the DSH upgrade first).**
- *Connection:* to the web host with its launch token (`GET /connect`, then the cookie).
- *Delivery:* `session/prompt` with `requestId` set to the delivery id, so native idempotency makes ambiguous retries safe. `mode:'steer'` for `next_step`, `mode:'queue'` for `next_turn`.
- *Busy or idle:* `session/state` / `session/observeState`, which exist only upstream. The installed rc.3 is 637 commits behind.

## First slice: gate-failure notices

The case that started this: tasks 10438 and 10441 had a failed `pair` gate, and
nothing told the session that submitted them. The Den tool even tells agents
not to watch gates, which is right, but it leaves them blind.

1. **Submission.** `SubmissionRequest` gains optional `notify_address`. The Den
   MCP `submit_task_for_review` schema gains the same field. That is a change
   in den-services, made separately.
2. **Producer.** crew-review sends as a service-owned origin
   (`system/crew-review`), not as an agent address.
3. **Outbox.** When the submission loop moves a submission to `gate_failed`
   (`submissionGateFailed` in `internal/review/submission.go`), it records a
   pending notice on the submission. The same loop then submits it to the
   fabric with `operation_id = submission/<id>/gate_failed/<gate handle>` until
   the fabric accepts it. Fabric idempotency makes retries and restarts safe
   without a transaction across the two databases. A re-run gate gets a new
   handle and therefore a new notice.
4. **Message.** The notice states the task, the commit, the failed check names
   with job links, and what to do: fix it, or re-run the check and resubmit
   with the same arguments. It is sent with `next_step` and a 24h TTL.
   *As built:* the outbox is `crew_review_notices` in the crew-review
   database (`pending` → `sent`, or `abandoned` when the fabric refuses the
   notice or has not accepted it within the 24h TTL). The gate handle is Den's
   gate ID plus `.<attempt>` from the second attempt on, since Den reopens the
   same gate ID on re-registration. crew-review registers as producer
   `crew-review` and binds `system/crew-review` to itself, which is all the
   fabric needs to accept that sender; no core change was required.
5. **Later notices.** `source_missing`, `stale`, `job_admitted` and review
   outcomes (`changes_requested`) can use the same outbox once the first one
   works.

The slice is done when a submission whose required check fails produces
exactly one notice per failed gate:
- a session that was idle wakes with it;
- a session that was mid-turn sees it at its next step;
- a session that had exited gets it on its next SessionStart.

Crew-review and the fabric must both be restartable at any point in between.

### Order of work

1. Fabric: the policy rename/extension and session-address adoption. Freeze
   the projection endpoints.
2. crew-review: `notify_address` and the notice outbox. Den schema field.
3. `crew-claude`, starting with a spike on the inbox socket protocol. Most
   submissions come from Claude Code sessions, so this makes the slice useful.
4. `crew-codex` onto the shared daemon.
5. `crew-dsh` after the DSH upgrade.

## Proof

- **State machine:** the policy × busy/idle/not-running table, and the
  per-gate idempotency of notices including restarts between outbox write and
  fabric accept.
- **Per driver:** a fake runtime endpoint plus one live check: an idle session
  wakes, a busy one sees the note at its next step, and nothing is cancelled.
- **End to end:** a deliberately failing required check on a scratch task.

## Open questions

- Retire the session projection, event log and SSE, or keep them frozen? I
  found no reader outside this repo.
- Is `next_turn` the right default for agent-to-agent messages, or should
  everything default to `next_step`?
- Is it acceptable to store the Claude messaging token in the fabric database
  (0600, loopback-only box)?
- Do Codex queued submissions survive a daemon restart? They are on disk,
  but this is untested.
- Once this is agreed: revise `AGENTS.md` (the "must not steer" rule and the
  wake-intent wording) and the spec's activation-policy and projection
  sections.
