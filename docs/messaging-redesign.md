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
  - `deliver_next_step`: `turn/steer` with the active turn id from `thread/turns/list`. Codex refuses a steer with no active turn (`-32600 no active turn to steer`); that falls back to `thread/queue/add`. An ambiguous steer never falls back. A `no_wake` message never falls back to the queue, since a queued item would start a turn on the idle thread: if the turn is gone before dispatch begins, the claim is released and the message stays pending for the next active turn; if Codex refuses the steer after dispatch began, nothing went in and the delivery is recorded `failed` (a begun dispatch cannot go back to the queue, and `outcome_unknown` would claim an ambiguity there is not).
  - `deliver_at_idle` and `register_next_turn`: `thread/queue/add`; the item leaves the queue and starts as soon as the thread is idle.
  - `wake_then_deliver` (`notLoaded`): queue, then `thread/resume`, then acknowledge. The wake stays owed until the thread is loaded: on every not-loaded pass (every 15th, and the first after a start) a known thread whose `thread/queue/list` still holds a `crew-delivery:*` item is resumed without queueing anything again. That covers a failed resume, a failed acknowledgement and a crash anywhere between queueing and resuming; items the user queued are not a reason to wake.
- *Reconcile:* `clientUserMessageId` is `crew-delivery:<delivery id>`. Codex keeps it as the user message's `clientId` for both steered and queued input, so a lost response is reconciled through `thread/queue/list` and the last turns. A dispatch left in flight by a crash shows up as the claim's `head_dispatching` head and is settled the same way.
- *Removed:* the private `codex app-server --stdio` child, the session projection writes, `thread/read includeTurns`, the control API (create thread, approvals and input requests, interrupt) and the `crew_directory`/`crew_message` dynamic tools. Those depended on owning the connection that started a thread; a TUI thread's approvals belong to the TUI. crew-review's ephemeral reviewer workers still use their own stdio app-server.
- *Findings (codex-cli 0.163.0):* a steer during a `sleep 20` shell command was recorded in the active turn at once and did not cut the command short. In one run the reply followed the note; in the fabric live check (with the `Crew message from` framing) the reply ignored it, so a steer is seen, not necessarily obeyed. Queued items are stored on disk (`~/.codex/queue_1.sqlite`), but surviving a daemon restart was not tested, since restarting the shared daemon would disturb the owner's sessions. A Codex PreToolUse hook can rewrite MCP tool input (`updatedInput` with `permissionDecision: allow`), so `notify_address` can be filled the same way as in Claude Code.
- *Live wake check (2026-10-29):* a scratch thread made with `codex exec` in a scratchpad directory (one turn, so it has a rollout) was registered by the SessionStart hook and read `notLoaded` in the daemon. A `next_turn` crew message to its `s/` address was claimed as `wake_then_deliver`, queued and resumed; Codex started the queued item at once (`client_id` `crew-delivery:<id>` on the user message) and the thread answered in a new turn, then went idle. Message to turn start took about 25 s, mostly waiting for the next not-loaded pass.

**DSH (`crew-dsh`, new; the local install was upgraded in #10450).**
- *Version:* the web host runs DSH 0.2.2-alpha.2 (`f3803c9697`). Types are in `packages/api/session-controller/src/types.ts`; the methods are in `src/index.ts` of the same package.
- *Connection:* `GET /connect?token=<launch token>&interactions=none` on the loopback host returns `302 ./api/remote.mux` and a signed cookie. `interactions` defaults to `none`; `answer` would make the driver a party to approvals and questions, which it should not be. The launch token is the one `dsh web` logs at start (`dsh-crew/get-token.sh` reads it from the journal), so it changes on every restart.
  - Unary calls are plain HTTP: `POST /api/<method>` with `{"type":"client-request","rpcId":…,"method":"<method>","payload":{"args":{…}}}`. `args` is keyed by the method's parameter name: `{"request":{…}}` for prompt, state, wait and events, and `{"_request":{}}` for `session/list`. The reply is `{"type":"server-response","rpcId":…,"result":{"ok":true,"value":…}}`, or `ok:false` with `error.code`.
  - Streams (`session/observeState`, `session/observeEvent`, `session/follow`) go over the `api/remote.mux` WebSocket with the same cookie. One mux per operator: a second mux on the same cookie gets 409, and closing the mux releases the operator, so its cookie then gets 401 on HTTP too.
- *Delivery:* `session/prompt` `{requestId, sessionId, mode:'queue'|'steer', content:[{type:'text',text}]}` returns `{accepted:true}`. `mode:'steer'` is for `next_step`, `mode:'queue'` for `next_turn`. On an idle session `queue` starts a turn at once. The method resumes a cold session before admitting the prompt (read from source, not tried).
- *Idempotency on `requestId`:* the host returns `accepted:true` without adding anything when the id is in the pending inbox or among the user messages in the loaded session history. Checked live: a repeat while the item was still queued, a repeat during the turn it started, and a repeat after that turn were all ignored. **There is a gap:** the turn takes the item out of the inbox (`agent/inbox/spliced removedCount:1`) about 0.1 s before it writes the `user/message`. A repeat 60 ms after the first call landed in that gap and ran as a second turn. The driver must not retry right after an ambiguous `prompt`. It should first look for the id with `session/state` (`queue[].rpcId`) and the recent `session/events` (`user/message` `data.source.rpcId`).
- *Busy or idle:* `session/state` `{sessionId}` returns `{cursor, active, running, queue, pendingInput}`. A cold session has `active:false, running:false, queue:null`. A loaded one has `queue:[{id, placement:'queued'|'steering'|'context', rpcId?, content}]`. `pendingInput` lists `approval`/`question`/`other` for open requests, and a change in it alone does not trigger `observeState`. `session/observeState` streams complete replacements of that state, starting with the current one. `session/wait` `{sessionId, timeoutMs? (default 30 s), afterSeq?, maxMessages?}` returns `{reason:'idle'|'input-required'|'timeout', state, output:{records, truncated}}`, where `output` holds the assistant messages after `afterSeq`. The wait never cancels anything. With no state before the deadline it fails with `session/wait-timeout`.
- *Events:* `session/events` `{address:{kind:'session',sessionId}, throughSeq?, beforeSeq?, maxMessages?}` returns a page that runs backwards from the log end, plus its `cursor`. `session/page` is the deprecated alias. Subagent children appear in `session/list` with `origin:'subagent'` and `parentSessionId`. Addressed as `kind:'session'` they fail with `session/agent-busy`, so they need `{kind:'subagent', parentSessionId, childSessionId, mode}`. The driver should skip them as delivery targets.
- *Old plugin:* the in-process `dsh-crew/plugins/crew-messaging` still builds against 0.2.2-alpha.2 and is still installed in the `web` profile, where it stays until the driver replaces it.

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
