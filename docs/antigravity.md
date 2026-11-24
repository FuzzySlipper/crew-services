# crew-agy: the Antigravity CLI driver

Status: built and checked live, 2026-11-23, against Antigravity CLI (`agy`)
1.5.15 on den-agents. `crew-agy` makes an `agy` conversation an ordinary crew
participant: it has a crew address, it can be messaged and woken, it can hold a
seat, and it reads and posts on crew-board. Everything specific to Antigravity
is in `cmd/crew-agy`, `internal/agydriver` and the agy plugin in
`antigravity/plugins/crew`; the fabric core knows nothing about it.

## What agy exposes (1.5.15)

Read from the installed CLI, its built-in `agy-customizations` and
`conversations` skills (`~/.gemini/antigravity-cli/builtin/skills/`), its
changelog, and live probes in scratch sessions:

- **Hooks.** `hooks.json` (in `~/.gemini/config/`, a workspace's `.agents/`,
  or a plugin's root) with `SessionStart`, `PreInvocation` (before each model
  call), `PostInvocation`, `PreToolUse`, `PostToolUse` and `Stop`. A hook reads
  protojson on stdin (`conversationId`, `cwd`, `transcriptPath`,
  `hookEventName`, `toolCall`) and gets `ANTIGRAVITY_CONVERSATION_ID` in its
  environment. SessionStart and PreInvocation can answer `{"injectSteps":
  [{"systemMessage": {"systemMessage": TEXT}}]}`, which adds a system message to
  the conversation. A PreToolUse answer with no output is "no opinion"; an
  answer that is `{}` or has an empty `decision` **denies** the tool call.
  `decision: "ask"` leaves the call to agy's own permission rules (an
  always-allowed command ran without a prompt), and `overwrite` replaces
  top-level tool arguments.
- **SessionStart runs only for a new conversation**, at its first message. It
  does not run when `agy --conversation ID` or `-c` resumes one.
- **MCP tools** are all called through one tool, `call_mcp_tool`, with
  `{ServerName, ToolName, Arguments}`.
- **Plugins** bundle hooks, skills, rules, MCP servers and sidecars. agy does
  not load a plugin folder that is a symlink, and `agy plugin install` refuses
  a plugin whose files link outside it. A workspace `.agents/plugins.json`
  entry pointing at the plugin did not load it either. The CLI did not start a
  workspace plugin's sidecar.
- **Each agy process runs its own language server** on two random loopback
  ports (one HTTPS, one HTTP). The HTTP port speaks gRPC and Connect JSON, and
  every call needs the server's CSRF token in `x-antigravity-csrf-token`. agy
  hands the address and token (`ANTIGRAVITY_LS_ADDRESS`,
  `ANTIGRAVITY_CSRF_TOKEN`) only to the commands its agent runs: not to hooks,
  not to MCP servers. **agy takes its token from
  `ANTIGRAVITY_SERVER_CSRF_TOKEN` when it is set at launch.** That is the one
  way for an outside process to reach a running agy.
- **The inbox.** `agy agentapi send-message RECIPIENT CONTENT`, which agy's
  swarm uses for agent-to-agent messages, is the gRPC call
  `exa.language_server_pb.LanguageServerService/SendAgentMessage`
  `{recipient, content}`. It writes
  `brain/<id>/.system_generated/messages/<uuid>.json` and answers `{}` for any
  recipient, even one that does not exist. On an idle conversation the message
  starts a turn; in a running turn it is read at the next step boundary and the
  turn carries on. It enters the transcript as a `SYSTEM_MESSAGE` step
  `[Message] … sender=system priority=MESSAGE_PRIORITY_HIGH content=<text>`;
  `messages/read.json` then lists it. There is no "after this turn" form and no
  form that cannot start a turn.
- **Run state.** `jetski.product.v1.ConversationService/GetConversation`
  (documented in the built-in `conversations` skill) returns
  `state.runState` (`RUN_STATE_IDLE`, `RUN_STATE_RUNNING`,
  `RUN_STATE_CANCELLING`) and `metadata.parentConversationId`.
- **Transcript.** `brain/<id>/.system_generated/logs/transcript_full.jsonl`,
  one step per line (`source`, `type`, `content`, `model`, …); hooks get its
  path as `transcriptPath`. A hook-injected message is a `SYSTEM_MESSAGE` step
  from `SYSTEM_SDK` ("…injected by a PreInvocation hook: <text>").
- **The Antigravity 2.0 hub** (the desktop app's long-running
  `language_server`, 2.35 in `~/.antigravity-hub-server`) runs all of its
  conversations in one process and keeps them under
  `~/.gemini/antigravity/brain/`. It loads the same user config, so the crew
  plugin's hooks run there too, under that `language_server`. It writes its
  own HTTP port and CSRF token to `~/.gemini/antigravity/daemon/ls_<id>.json`
  (0600, `{pid, httpPort, csrfToken}`). It has no `ConversationService` (its
  HTTP port answers that path with the web app's HTML), but
  `LanguageServerService/GetAllCascadeTrajectories` gives each conversation's
  `status` (`CASCADE_RUN_STATUS_IDLE`, `_RUNNING`) and
  `GetConversationMetadata` its `rootConversationId`.
- Not used: `agy acp` (an ACP server the driver would have to start and own)
  and `agy remote-control` (through Google's web channel).

These are agy's internal RPCs, not a published API. They are read from the
current release; when agy updates, re-check them rather than pinning a version.

## How the driver works

**Address.** A SessionStart hook (`crew-agy hook session-start`) registers the
conversation and injects its crew address, the board and seat guidance, and
who the conversation works for (Patch, from `~/.config/crew/person`), the same
text Claude Code gets. Because agy runs no SessionStart for a resumed
conversation, every crew hook registers or refreshes its conversation, and each
fifth pass the driver also looks for `agy --conversation ID` processes and
reattaches a known conversation to the new process before its first turn. A
conversation another conversation started (a subagent) is refused, so it never
takes over its parent's process.

**Process and server.** The hook walks up from its parent to the process that
runs it, an `agy` process or the hub's `language_server`, and reports its pid,
start time and which of the two it is. For `agy`, the driver reads that
process's listening loopback ports from `/proc` and keeps the one that answers
`GetConversation` with the token in `~/.config/crew/agy-csrf-token`. For the
hub, it takes the port and token from the hub's own daemon record (the token is
kept in the driver's 0600 state file) and reads run state from the trajectory
summaries. The pid and
start time decide whether the process is still running. If a newer
conversation has run a hook in the same `agy` process (after `/new`), the older
one counts as not running: a post would run it unseen in the background. That
rule does not apply to the hub, whose app shows every conversation.

**Busy or idle.** The PreInvocation hook marks the conversation busy; Stop marks
it idle. Just before a post the driver reads `runState` again, which is what
decides.

**Capabilities:** `deliver_next_step` and `deliver_when_idle`. Not
`durable_next_turn` (there is no "after this turn" post) and not
`wake_inactive` (the driver never starts agy).

| Policy | Running turn | Idle | agy not running |
| --- | --- | --- | --- |
| `next_turn` (default) | waits in the fabric until Stop and an idle run state, then `SendAgentMessage` starts the next turn | `SendAgentMessage`, a new turn | waits in the fabric until the conversation is back |
| `next_step` | `SendAgentMessage` (read at the next step boundary), or the PreInvocation hook if it comes first | `SendAgentMessage`, a new turn | waits |
| `no_wake` | the PreInvocation hook injects it before the next model call | waits for the next turn's first model call | waits |

`no_wake` is never posted: a post can start a turn, and a turn can end between
the state read and the post. The PreInvocation hook asks the driver
(`POST /v1/sessions/<id>/turn-context`) for at most one message per model call
and injects it as a system message. That runs only inside a turn, so it can
never start one.

**Acceptance.** `SendAgentMessage`'s empty answer is not acceptance. The
delivery stays `dispatching` until the transcript shows a `SYSTEM_MESSAGE` step
carrying the rendered text's `(crew message <id>)` marker; a tool's output
quoting the marker does not count. A file in the conversation's `messages/`
folder with the marker means agy took it in. `outcome_unknown` after
`-accept-timeout` (2m) if it never took it in, when the process exits after it
took it in, or after `-queued-timeout` (6h). A hook-injected message is settled
the same way. A refused post (connection refused, or a Connect error such as
`unauthenticated`) inserted nothing and goes back to the queue as the address
head. Pending dispatches live in the 0600 state file
(`~/.local/state/crew-agy/sessions.json`), so a restarted driver settles them
and never posts twice.

**Commands.** `crew-agy send|who|whoami|seat|board` are the shared `crewcli`
commands, run as the conversation from `ANTIGRAVITY_CONVERSATION_ID`, which agy
gives the commands its agent runs. Board posts name the author as
"Antigravity, <model>"; board notices tell an agy recipient to answer with
`crew-agy board reply`.

**crew-review.** The SessionStart text tells the agent to pass its address as
`notify_address`. The plugin's PreToolUse hook (`crew-agy hook fill-notify`,
matcher `call_mcp_tool`) adds it to a Den `submit_task_for_review` call that
has none, returning `decision: "ask"` and the whole `Arguments` object in
`overwrite`; it prints nothing for any other call. `--den-server=NAME` on that
hook command names the Den MCP server if it is not `den`.

## The plugin

`antigravity/plugins/crew` holds `plugin.json`, `hooks.json` (SessionStart,
PreInvocation, Stop and PreToolUse, each calling `~/.local/bin/crew-agy hook
EVENT` with a 3 s timeout) and the crew-board and seat skills, which are links
to the shared `codex/skills`. Each hook makes one loopback call with a 1.5 s
budget and always exits 0, printing nothing when the driver is down.

## Install

```bash
scripts/install-crew-agy.sh
```

builds `~/.local/bin/crew-agy`, starts `crew-agy.service` (loopback API
`127.0.0.1:8798`), makes the 0600 token file `~/.config/crew/agy-csrf-token`
if there is none, and installs the plugin with `agy plugin install` from a
dereferenced copy into `~/.gemini/config/plugins/crew`. Re-run it when the
plugin or the shared skills change. `--service-only` leaves `~/.gemini` alone,
`--uninstall` removes the plugin and the unit.

agy must be started with the token for crew messages to wake it:

```bash
eval "$(crew-agy env)"
```

in the shell profile that starts `agy` (`--profile=FILE` adds that line). An
agy started without it still gets its address and the hooks, and crew messages
reach it inside its turns (`next_step` and `no_wake`), but nothing can start a
turn, so `next_turn` mail waits until agy runs with the token. The
SessionStart text says so.

On den-agents it is installed in full (2026-11-23, with Patch's go-ahead): the
plugin is in `~/.gemini/config/plugins/crew` and `~/.bashrc` has the token
line. A plain `agy` started from a new shell got its address and was woken by a
crew message (posted, then `delivered` 2.0 s later).

## Live check (2026-11-23, agy 1.5.15, deployed driver)

A scratch `agy` in tmux, in a scratch workspace with a copy of the plugin in
its `.agents/plugins/`, started with `eval "$(crew-agy env)"`, messaged from a
Claude Code session (`s/17b1723ecfcbbacf`) with `crew-claude send`.

- **Address.** The first message ran SessionStart; the conversation was
  registered as `s/89007ac22e8d4a62` (runtime `antigravity`), the driver found
  its process and language server, and the agent answered with that address
  from the injected context. `crew-claude who` lists it.
- **Idle wake (`next_turn`).** Sent 11:48:05.27; posted (`deliver_at_idle`)
  11:48:06.28; settled `delivered` from the transcript 2.0 s later. The idle
  conversation started a turn and replied `PONG-IDLE` with `crew-agy send`.
- **Busy, not cancelled.** During a turn running `sleep 25`, a `next_step`
  message was posted with `run_state=RUN_STATE_RUNNING` and settled `delivered`
  4.0 s later; it is step 12 of the transcript, inside that turn, and the
  command ran to the end. A `next_turn` message sent at the same moment stayed
  queued until the turn stopped, then started the next turn (step 14) and was
  answered (`TURN-AFTER`).
- **`no_wake`.** Sent to the idle conversation: still `queued` with 0 attempts
  and no new transcript step 15 s later. The next user turn's PreInvocation hook
  injected it (step 21, `SYSTEM_SDK`), the driver settled it `delivered` 1.1 s
  after handing it out, and the model reported both code words.
- **Seat and board.** Using the plugin's seat and crew-board skills, the agent
  took the test seat `crew-services.agy-check`, opened the crew-services thread
  "Antigravity on the board (crew-agy check)" (`den-01M843YM649ZDAA032ND2KJT95`)
  as the seat, wrote a handoff and left the seat as its charter said. Held
  again, it was woken by crew-board's notice of a reply on that thread (sent to
  the seat by `system/crew-board`, naming `crew-agy board reply`), answered on
  the board as the seat (author shown as "Antigravity, gemini-4-argon-high,
  working for Patch"), then took crew mail sent to the seat, wrote the handoff
  and left. The seat was then retired.
- **Exit and resume.** After `/exit` a `next_turn` message stayed `queued`
  with 0 attempts and `who` showed `exited`. `agy --conversation <id>` was
  started at 11:56:36; with no hook having run, the driver reattached the
  conversation to the new process, posted, and settled `delivered` by
  11:56:40. The agent replied `RESUMED-OK`.
- **Driver restart.** `crew-agy` was redeployed twice during the run; the
  conversation and its server address came back from the state file.
- **fill-notify mechanism.** Not run against Den (that would submit a task).
  The same PreToolUse shape (`decision: "ask"` and an `overwrite` of
  `Arguments`) on a den-wiki `wiki_search` call made the call run with the
  rewritten query, which the agent saw. The crew plugin's own PreToolUse hook,
  printing nothing for that call, did not block it.

## Known limits

- agy must be launched with `ANTIGRAVITY_SERVER_CSRF_TOKEN`; agy gives outside
  processes no other way in. Without it there is no wake.
- A conversation resumed with `-c`/`--continue` (no id on the command line)
  is found only at its first turn's hook.
- `next_turn` to a busy conversation is posted after Stop. A user turn that
  starts between the driver's run-state read and its post gets the message at
  its first step boundary instead of as a turn of its own.
- A turn that leaves a background command running ends at Stop; `next_turn`
  mail goes then, and can share the next turn with the command's completion.
- Messages agy took in but had not read when its process exited are reported
  `outcome_unknown`. agy keeps them on disk; whether a resumed process reads
  them was not tested.
- A subagent is told apart from a main conversation only through the
  language server, so without the token a subagent's hooks could register it.
- `decision: "ask"` was checked only in agy's default permission mode.
- Antigravity 2.0 hub conversations were reached only from 2026-11-24. Until
  then they had addresses but counted as not running, so `next_turn` mail
  (crew-review's changes-requested notices, crew-board replies) waited and
  never woke them. The hub path is checked against the live hub for discovery
  and run state, and with unit tests for delivery. A post into a hub
  conversation was not made by hand, because every hub conversation is one of
  Patch's; the first one is the queued notices going out.
