# Messaging redesign: durable core, thin drivers

Status: built, 2026-10-29. Proposed on 2026-10-28 and implemented under campaign
#10448 (#10449–#10456); the sections below describe what was built, with
*as built* notes where it differs from the proposal. Lasting contract rules
live in the [messaging service specification](messaging-service-spec.md) and
`AGENTS.md`; where this note and the spec disagree, the spec is what the code
does. End-to-end evidence: [gate-failure slice evidence](evidence/gate-failure-slice.md).

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
| DSH (upstream 0.2.2-alpha.2) | web host `session/prompt` `mode:'steer'`; without waking, `agent.inject` through the dsh-crew-inject plugin | `session/prompt` `mode:'queue'`; resumes a cold session | `accepted:true`; idempotent on `requestId` except in a ~0.1 s inbox-to-history window |

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
4. **The transcript mirror went.** The session projection, event log and SSE
   replay held 1,619 events, the last from 2026-09-04, and copied runtime
   state the runtimes already keep. Their only client was the retired
   dsh-crew `crew-messaging` plugin, which #10454 removed from the DSH profile
   and `update.sh`. They were deleted on 2026-10-29 (#10456): the routes, code
   and tables are gone (migration 9). The drivers' own `GET /v1/sessions` are
   separate driver APIs and stay.
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
  - The session answers nothing on the connection. Held, denied, refused and expired messages are reported only as `peer_message_status` frames to the sender's `from` reply socket, which the driver doesn't have.
  - `next` is read between tool calls of an active turn. `later` is held until the current turn ends. Both start a turn on an idle session. `now` preempts and is never used.
  - No frame enters only an active turn. The frame accepts nothing besides content, priority, msg_id, from, uuid and file attachments (`passive` and `wake:false` exist only for in-process poll events).
- *Registration:* a user-level SessionStart hook (`crew-claude hook session-start`) sends the session id, socket, token and `transcript_path` to the driver and puts the session's address into context. The socket and token change on every process restart, so they stay in the driver's 0600 state file (`~/.local/state/crew-claude/sessions.json`), not in the fabric binding. `claude --resume` keeps the session id, so a resumed session keeps its address.
- *Busy or idle:* UserPromptSubmit marks the session busy (it also fires for a turn a crew message started), and so does each PostToolUse. Stop and StopFailure mark it idle; a turn that ends on an API error fires only StopFailure. A session with no socket file is not running.
- *Capabilities:* `deliver_next_step` and `deliver_when_idle`. Not `durable_next_turn`: Claude Code's follow-up queue is in memory and an exit loses it. So a `next_turn` message for a busy session stays `queued` in the fabric until the Stop hook reports idle. While it waits it also holds back later messages to that address, because the fabric delivers in order.
- *Delivery:*
  - The turn state is read for each claim and again just before the write.
  - `no_wake` never goes to the inbox socket. Every inbox frame can start a turn: a `next` frame that lands during a turn's final, tool-free response is not absorbed and starts a new turn after Stop (the 07:42:53 live check below did this with a `next_step` message). A delivery pass that claims a `no_wake` message releases it and leaves that address alone for 30s or until the session's next hook.
  - `no_wake` goes out only through *in-turn hooks*, whose output Claude Code adds to a turn that is already running and which can never start one: UserPromptSubmit (`crew-claude hook busy`, as a turn starts) and PostToolUse (`crew-claude hook step`, matcher `*`, after every tool call). The hook calls the driver's `POST /v1/sessions/<id>/turn-context`. The driver claims the address's head as busy (`deliver_next_step`), begins dispatch, records a pending dispatch, and returns the rendered message, which the hook prints as `hookSpecificOutput.additionalContext`. A `next_step` message the hook claims goes the same way. A hook from a subagent (`agent_id` in its input) gets nothing, since its context and transcript are the subagent's. One message per hook call: the fabric dispatches one message per address at a time, so the next call settles the previous one from the transcript first and then hands out the next. The installer adds the PostToolUse hook; until it is re-run, `no_wake` reaches a session only as a turn starts.
  - `deliver_next_step` for `next_step` from a delivery pass goes out as `next`; landing in the final response, it starts a turn after Stop, which `next_step` allows.
  - `deliver_at_idle` goes out as `later`. If the session became busy first, a `next_turn` claim is released (a `next_step` claim goes out as `next`).
  - The driver dials before it records the dispatch, so a session that is gone sends the claim back to the queue.
- *Acceptance:* a successful socket write is not acceptance. The session writes what its inbox did to the transcript (`~/.claude/projects/<cwd with non-alphanumerics as '-'>/<session id>.jsonl`):
  - When the inbox admits the frame, it writes `{"type":"queue-operation","operation":"enqueue","content":"<text>"}` (with no msg_id).
  - When the message enters the conversation, it writes a line with `origin.msg_id` equal to the frame's `msg_id`. That is an `attachment` of type `queued_command`, followed by `queue-operation remove` with `absorbed_mid_turn`, when an active turn takes it. It is a `user` line with `turnOrigin: "peer"` when it starts a turn.
  - Hook context is recorded as `{"type":"attachment","attachment":{"type":"hook_additional_context","content":["<text>"],"hookName":"PostToolUse:Bash","hookEvent":"PostToolUse","toolUseID":…}}` (or `UserPromptSubmit`), written right after the hook returns and rendered to the model as a `<system-reminder>` reading `PostToolUse:Bash hook additional context: <text>` in the turn's next request. It has no msg_id, so the driver matches the `(crew message <id>)` marker in its content; a quote of the text in a tool result does not count. A hook dispatch the transcript never shows (the hook timed out, the process died) becomes `outcome_unknown` after `-accept-timeout` and is never handed out again.
  - The delivery stays `dispatching` until that msg_id line appears, and is then acknowledged. The driver reads only lines added after the write, and the msg_id quoted in a tool result does not count.
  - It becomes `outcome_unknown` in three cases: the session never queued the message within `-accept-timeout` (2m; held, refused, dropped, or the write was lost); the session queued it, then its process exited or was replaced; or the message stayed queued past `-queued-timeout` (6h).
  - Pending dispatches are kept in the state file, so a restarted driver settles them the same way. A dispatch it has no record of is checked against the whole transcript. Nothing is ever posted twice.
  - Once the transcript shows the message, the dispatch is saved as `inserted` before the acknowledgement goes out. The read offset has already moved past that line, so a failed acknowledgement is retried from the saved fact, with the same operation ID (`crew-claude/ack/<delivery id>`), also after a driver restart; the fabric replays it if the first one landed and only its response was lost.
- *Inbound control:* the installer sets `crossSessionInbound: accept`. Without it, a session that bypasses permission prompts holds the driver's messages for approval.
- *Ordering:* the fabric hands out an address's messages in order, one head at a time. A `no_wake` behind a `next_turn` head waits while that head waits for idle; after the `next_turn` message starts its turn, that turn's UserPromptSubmit carries the `no_wake`. A `no_wake` head on an idle session holds back everything behind it, including `next_turn` work, until a turn starts (the user's or another runtime's); then its hooks drain the head and the rest follows the usual rules. A turn with no tool calls hands out at most the one message its UserPromptSubmit carries.
- *Residual window (not closed):*
  - A message the session has queued but not yet taken in is lost if the process exits. The driver reports it as `outcome_unknown`, never `delivered`.
- *Live checks (2026-10-29, Claude Code 2.1.310, real hooks, deployed driver):*
  - **Idle wake.**
    - A scratch `claude` session in tmux was sent `next_turn`. The driver posted `deliver_at_idle`/`later` 0.6s after the send. About 40ms later the UserPromptSubmit hook marked the session busy. The transcript shows a `turnOrigin: "peer"` user line with the msg_id, and the delivery was acknowledged 2.0s after the post.
    - The scratch CLI was not logged in, so the model call failed ("Login expired"), and that turn ended in StopFailure and not Stop.
    - With a working model, the coordinator session woke from idle on a crew message at 07:13:59Z and answered it.
  - **Busy, non-cancelling.**
    - An earlier run (03:03Z): a `next_step` message reached the coordinator mid-turn as a `queued_command` attachment, then `absorbed_mid_turn` 2.1s after enqueue, and the turn carried on.
    - Again at 07:42:53Z, through the new driver: the coordinator was already writing its final response, so the message was enqueued at once and started a new turn 3.2s later, right after Stop. That shows the window above.
    - That delivery was wrongly settled `outcome_unknown` by the first deployed build. Sessions registered before the change had no transcript path, so it read nothing. It is fixed in 322c473, which falls back to the default path.
  - **Exited, then resume.**
    - The scratch session exited (`/exit`, socket gone). A `next_step` message sent then stayed `queued` with 0 attempts for 18s.
    - `claude --resume <id>` registered the same address with a new socket. The driver posted 0.4s after launch, the transcript showed it 0.37s after the post, and it was acknowledged 1.1s after the post.
  - **`no_wake` through PostToolUse (Claude Code 2.1.312, real hooks, deployed driver, model replaced by a local mock of the Messages API so a scratch `claude -p` could run a tool).** The mock answered with a `sleep 4` Bash call; a `no_wake` message was sent 0.5s into the turn. A delivery pass claimed and released it (fabric `attempt_count` 2) and posted nothing. The PostToolUse hook got it 4.2s later; Claude Code recorded the hook at 27ms (sender lookup included), wrote the `hook_additional_context` attachment, and sent `PostToolUse:Bash hook additional context: Crew message from …` to the model as a system message in the same turn's next request. The driver settled it `delivered` 247ms after handing it out. The transcript shows no `enqueue` or inbox entry for it.
  - **In-turn hook latency (2026-10-29, 20 runs each, nothing pending):** `hook step` median 6.0ms (5.1–8.2), `hook busy` median 6.5ms (5.0–7.4); of that, the driver's `turn-context` call (one fabric claim) is median 1.1ms (0.7–1.6) and process start is about 5ms on this host at the time. A hook that prints nothing leaves no transcript entry.
  - **Hook latency.** `crew-claude hook busy|idle|session-start` was run with realistic stdin, 10 runs each:
    - busy: median 2.6ms (2.1–3.7).
    - idle: median 2.4ms (2.0–3.4).
    - session-start: median 2.7ms (2.3–3.4).
    - Claude Code recorded 4–7ms for the Stop hook on 14 turns of the coordinator session. With the driver down, a hook returns in about 2ms (connection refused).
  - **Not checked live:** a turn-start delivery with a working model in a scratch session (the scratch CLI could not log in), and `no_wake` with a real model (the PostToolUse run above used a mock model).

**Codex (`crew-codex`, reworked in #10453).**
- *Connection:* the daemon's control socket speaks WebSocket (one JSON-RPC message per text frame; `codex app-server proxy` only forwards bytes), so the driver dials it directly. It never starts, stops or restarts the daemon or a thread, and never uses the `/daemon/shutdown` path.
- *Registration:* every loaded thread gets an `s/...` session address, found through `thread/loaded/list` each pass and the `thread/started` notification, which reaches every connection. Ephemeral threads and threads with `canAcceptDirectInput: false` are skipped. A SessionStart hook (`crew-codex hook session-start`) registers the thread too and returns its address as additional context. Named `-address` pins stay; one already bound to `crew-codex` keeps its target and generation.
- *Busy or idle:* `thread/read` status each pass (`active`, `idle`, `notLoaded`; `systemError` holds the work). `thread/status/changed` also reaches unsubscribed connections and triggers a pass.
- *Delivery:*
  - `deliver_next_step`: `turn/steer` with the active turn id from `thread/turns/list`. Codex refuses a steer with no active turn (`-32600 no active turn to steer`); that falls back to `thread/queue/add`. An ambiguous steer never falls back. A `no_wake` message never falls back to the queue, since a queued item would start a turn on the idle thread: if the turn is gone before dispatch begins, the claim is released and the message stays pending for the next active turn; if Codex refuses the steer after dispatch began, the driver checks that the message is not in the thread (`thread/queue/list` and recent turns) and reports the attempt *not inserted* (reason `steer_rejected`), which returns the delivery to the fabric queue as the address head; a later active turn claims it again and it is steered in once. If Codex cannot be checked the outcome is `outcome_unknown`. Each claim attempt uses its own begin and not-inserted operation ids (`crew-codex/begin/<delivery>[/<attempt>]`).
  - `deliver_at_idle` and `register_next_turn`: `thread/queue/add`; the item leaves the queue and starts as soon as the thread is idle.
  - `wake_then_deliver` (`notLoaded`): queue, then `thread/resume`, then acknowledge. The wake stays owed until the thread is loaded: on every not-loaded pass (every 15th, and the first after a start) a known thread whose `thread/queue/list` still holds a `crew-delivery:*` item is resumed without queueing anything again. That covers a failed resume, a failed acknowledgement and a crash anywhere between queueing and resuming; items the user queued are not a reason to wake.
- *Reconcile:* `clientUserMessageId` is `crew-delivery:<delivery id>`. Codex keeps it as the user message's `clientId` for both steered and queued input, so a lost response is reconciled through `thread/queue/list` and the last turns. A dispatch left in flight by a crash shows up as the claim's `head_dispatching` head and is settled the same way.
- *Removed:* the private `codex app-server --stdio` child, the session projection writes, `thread/read includeTurns`, the control API (create thread, approvals and input requests, interrupt) and the `crew_directory`/`crew_message` dynamic tools. Those depended on owning the connection that started a thread; a TUI thread's approvals belong to the TUI. crew-review's ephemeral reviewer workers still use their own stdio app-server.
- *Findings (codex-cli 0.163.0):* a steer during a `sleep 20` shell command was recorded in the active turn at once and did not cut the command short. In one run the reply followed the note; in the fabric live check (with the `Crew message from` framing) the reply ignored it, so a steer is seen, not necessarily obeyed. Queued items are stored on disk (`~/.codex/queue_1.sqlite`), but surviving a daemon restart was not tested, since restarting the shared daemon would disturb the owner's sessions. A Codex PreToolUse hook can rewrite MCP tool input (`updatedInput` with `permissionDecision: allow`), so `notify_address` can be filled the same way as in Claude Code.
- *Live wake check (2026-10-29):* a scratch thread made with `codex exec` in a scratchpad directory (one turn, so it has a rollout) was registered by the SessionStart hook and read `notLoaded` in the daemon. A `next_turn` crew message to its `s/` address was claimed as `wake_then_deliver`, queued and resumed; Codex started the queued item at once (`client_id` `crew-delivery:<id>` on the user message) and the thread answered in a new turn, then went idle. Message to turn start took about 25 s, mostly waiting for the next not-loaded pass.

**DSH (`crew-dsh`, built in #10454; the local install was upgraded in #10450).**
- *Version:* the web host runs DSH 0.2.2-alpha.2 (`f3803c9697`). Types are in `packages/api/session-controller/src/types.ts`; the methods are in `src/index.ts` of the same package.
- *Connection:* `GET /connect?token=<launch token>&interactions=none` on the loopback host returns `302 ./api/remote.mux` and a signed cookie. `interactions` defaults to `none`; `answer` would make the driver a party to approvals and questions, which it should not be. The launch token is the one `dsh web` logs at start (`dsh-crew/get-token.sh` reads it from the journal), so it changes on every restart.
  - Unary calls are plain HTTP: `POST /api/<method>` with `{"type":"client-request","rpcId":…,"method":"<method>","payload":{"args":{…}}}`. `args` is keyed by the method's parameter name: `{"request":{…}}` for prompt, state, wait and events, and `{"_request":{}}` for `session/list`. The reply is `{"type":"server-response","rpcId":…,"result":{"ok":true,"value":…}}`, or `ok:false` with `error.code`.
  - Streams (`session/observeState`, `session/observeEvent`, `session/follow`) go over the `api/remote.mux` WebSocket with the same cookie. One mux per operator: a second mux on the same cookie gets 409, and closing the mux releases the operator, so its cookie then gets 401 on HTTP too.
- *Delivery:* `session/prompt` `{requestId, sessionId, mode:'queue'|'steer', content:[{type:'text',text}]}` returns `{accepted:true}`. `mode:'steer'` is for `next_step`, `mode:'queue'` for `next_turn`. On an idle session `queue` starts a turn at once. The method resumes a cold session before admitting the prompt (checked live in #10454).
- *Idempotency on `requestId`:* the host returns `accepted:true` without adding anything when the id is in the pending inbox or among the user messages in the loaded session history. Checked live: a repeat while the item was still queued, a repeat during the turn it started, and a repeat after that turn were all ignored. **There is a gap:** the turn takes the item out of the inbox (`agent/inbox/spliced removedCount:1`) about 0.1 s before it writes the `user/message`. A repeat 60 ms after the first call landed in that gap and ran as a second turn. The driver must not retry right after an ambiguous `prompt`. It should first look for the id with `session/state` (`queue[].rpcId`) and the recent `session/events` (`user/message` `data.source.rpcId`).
- *Busy or idle:* `session/state` `{sessionId}` returns `{cursor, active, running, queue, pendingInput}`. A cold session has `active:false, running:false, queue:null`. A loaded one has `queue:[{id, placement:'queued'|'steering'|'context', rpcId?, content}]`. `pendingInput` lists `approval`/`question`/`other` for open requests, and a change in it alone does not trigger `observeState`. `session/observeState` streams complete replacements of that state, starting with the current one. `session/wait` `{sessionId, timeoutMs? (default 30 s), afterSeq?, maxMessages?}` returns `{reason:'idle'|'input-required'|'timeout', state, output:{records, truncated}}`, where `output` holds the assistant messages after `afterSeq`. The wait never cancels anything. With no state before the deadline it fails with `session/wait-timeout`.
- *Events:* `session/events` `{address:{kind:'session',sessionId}, throughSeq?, beforeSeq?, maxMessages?}` returns a page that runs backwards from the log end, plus its `cursor`. `session/page` is the deprecated alias. Subagent children appear in `session/list` with `origin:'subagent'` and `parentSessionId`. Addressed as `kind:'session'` they fail with `session/agent-busy`, so they need `{kind:'subagent', parentSessionId, childSessionId, mode}`. The driver should skip them as delivery targets.
- *Old plugin:* the in-process `dsh-crew/plugins/crew-messaging` is retired: removed from the `web` profile and from `dsh-crew/update.sh`, which now builds and restarts `crew-dsh` (dsh-crew 2eecab0). Its reviewer-runtime route, used only by crew-review's optional `dsh` backend (the service runs `codex`), went with it.
- *Non-waking insert (R10454-1):* the agent loop has `send(message, target, wakeup)`, with `steer = send(m,'next-step',true)` and `inject = send(m,'next-step',false)` (`packages/core/agent-loop/src/agent.ts`). The session API exposes only the waking forms, so a steer that the driver sent for a turn it had read as running would start a new turn if that turn ended first. The dsh-crew `plugins/crew-inject` plugin (`dsh-crew-inject`, in the `web` profile; dsh-crew `update.sh` builds and re-adds it) registers one exact route with `ctx.connection.fetch.register`: `POST /api/crew-inject/inject` `{sessionId, requestId, content}`. That puts it behind the host's own trust and cookie check, and only owner peers reach it. The route finds the live root agent and calls `agent.inject` with a user message whose source is `{kind:'user', rpcId: requestId}`, the shape `session/prompt` writes. It first applies `hasPromptRequest`'s check (either inbox, or a `user/message` in the history) and answers `accepted:true` without a second insert. The check and the insert run with no await between them. An inject never wakes the agent. A turn in its last step takes it at the re-check after `agent/turn-stopping` and runs another step. One past that point leaves it pending, and the agent goes idle. The next turn's first step claims every next-step item along with its own message (`inbox.claim`). A user cancel keeps the inbox (`keepInbox:true`).
- *As built:*
  - *Registration:* discovery through the web host, not a plugin or hook. Every root `session/list` row (no `origin:'subagent'`/`parentSessionId`, not blank, with a cwd, updated within `-recent` 14 days) gets an `s/...` address; a session keeps it while the host lists it. `crew-dsh address` (from `DSH_SESSION_ID`, which DSH sets for its shell tools) registers a session on demand, and a subagent resolves to its root. No SessionStart context: the hook bridge is not in the web profile.
  - *Busy or idle:* per pass from `session/list` (`agentAvailable:false` is not loaded, `running` is busy). Not-loaded sessions are claimed every 5th pass (10 s).
  - *Capabilities:* `deliver_next_step`, `deliver_when_idle`, `durable_next_turn` (the inbox is persisted in the session log as `agent/inbox/spliced`, read from source) and `wake_inactive`.
  - *Delivery:* `requestId` is `crew-delivery:<delivery id>`. `next_step` steers, `next_turn` queues, whatever the fabric action, and `no_wake` goes through the inject route, never `session/prompt`. The session state is read after the claim (an unreadable host releases the claim). The fabric only hands out `no_wake` while the session is busy. The race is gone because the driver no longer re-checks `running`: an inject that lands after the turn ended waits for the next turn. A 404 from the route (plugin not installed) is *not inserted* (`inject_unavailable`) with the session backoff, and is never retried as a steer. An inject's ambiguous outcome is reconciled like a prompt's: the message shows up in `state.queue[].rpcId` (placement `steering`) and as an `agent/inbox/spliced` `next-step` insert.
  - *Ambiguity:* an ambiguous `prompt` is never repeated at once. The driver waits `-reconcile-wait` (1 s), then looks for the id in `state.queue[].rpcId` and the last 40 messages of `session/events` (`agent/inbox/spliced` `inserted[].source.rpcId`, which is written at admission and so also covers the gap, and `user/message` `source.rpcId`). Found: acknowledge. Absent: one re-send with the same id, unless the first call timed out (the host may still be resuming the session), which is `outcome_unknown`, as is a second unclear answer or an unreadable session. An `ok:false` refusal of a prompt the session does not hold is *not inserted* (`prompt_refused`) with a 30 s–10 m backoff for that session; `session/not-found` fails the delivery. A crash-left `head_dispatching` dispatch is checked the same way and never re-sent.
- *Live checks (2026-10-29, deployed driver, scratch sessions under the scratchpad):*
  - **Idle wake.** A `next_turn` message to an idle session was acknowledged 0.76 s after `crew-dsh send` (`deliver_at_idle`, `queue`); the turn started 18 ms before the acknowledgement, `user/message` carried `rpcId crew-delivery:<id>`, and the session answered.
  - **Busy, non-cancelling.** During a user turn running `sleep 20`, a `next_step` message was steered in (`agent/inbox/spliced` `next-step` at +12.9 s into the tool call). The tool ran its full 20 s; the item left the inbox at the step boundary right after the tool result and became step 2's `user/message` in the same turn, which completed normally (`turn/end` `completed`). The model read it (and declined the embedded instruction as untrusted).
  - **Cold resume.** After a `dsh web` restart (which also loaded the profile without the old plugin) the session read `not loaded`. A `next_turn` message was claimed as `wake_then_deliver`, the prompt resumed it and started a turn; acknowledged 0.1 s after the send. The driver had reconnected with the new launch token on its own.
  - **`no_wake` (before R10454-1).** Sent to an idle session, it stayed queued 8 s with no turn and no prompt. When a user turn with a `sleep 8` tool call started, the driver steered it in 0.68 s later; it became step 2's `user/message` of that turn, which completed.
  - **`no_wake` through inject (R10454-1, scratch session A `s/711a4c1647dd319f`).** *Idle:* a fabric `no_wake` stayed `queued` with no dispatch while A was idle (cursor unchanged). An inject straight into the idle agent was accepted, and its repeat came back `replayed`. The log gained only `agent/inbox/spliced next-step [live-inject-idle-1]`, and `session/state` showed it as `queue[{placement:'steering', rpcId}]` with `running:false`: no turn after 6 s. The next turn (a user prompt) claimed it at step 1 along with its own message. Both are `user/message` at step 1 of turn 10. In that same turn the driver injected the fabric-held `no_wake` (`deliver_next_step`, `native=accepted`), and step 2 read it. *Running:* during turn 11's `sleep 20` step, a `no_wake` sent 4 s in was spliced into next-step and became step 2's `user/message`. `turn/end` was `completed`, and no further turn started.
  - **Retry after an ambiguous outcome** (`go test -tags dshlive`, real host, the driver's dispatch with a wrapper that makes the first prompt ambiguous). *Response lost inside the window:* the prompt was accepted and the wrapper reported a reset at once, while the turn was taking the item; the driver waited 1 s, found it and acknowledged without re-sending: one `turn/start`, one `user/message` with the id. *Request lost:* the check found nothing, the driver re-sent once and acknowledged: one turn, one `user/message`.

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
   notice or has not accepted it within the 24h TTL). A notice whose recipient
   the fabric does not know (`not_found`/`not_bound`) is rescheduled with its
   own backoff (`next_attempt_at`, 30s doubling to 15m) and the pass moves on;
   only a fabric-wide failure stops a pass, and the failed notice is requeued
   behind the others that are due. The gate handle is Den's
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

All five steps are done (#10449, #10451/#10455, #10452, #10453, #10450/#10454).

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
  *As built:* recorded in [evidence/gate-failure-slice.md](evidence/gate-failure-slice.md)
  (#10456): busy, idle, exited-then-resumed, restarts with the fabric down,
  and a passing re-run, for Claude Code; idle and busy for Codex and DSH.

## Open questions

Resolved during the campaign:

- *Retire the session projection?* Yes. Nothing read it after 2026-09-04 and its
  only client was the retired dsh-crew plugin; migration 9 dropped its tables
  (#10456).
- *Store the Claude messaging token in the fabric?* No. The token stays in
  crew-claude's own state file (`~/.local/state/crew-claude/sessions.json`,
  0600); the fabric never sees runtime credentials.
- *Revise `AGENTS.md` and the spec?* Done: waking, steering and interrupting are
  separate concepts, and the spec carries the activation table and the removed
  projection.

Still open:

- Is `next_turn` the right default for agent-to-agent messages, or should
  everything default to `next_step`?
- Do Codex queued submissions survive a daemon restart? They are on disk, but
  this is untested.
