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

The fabric records durable delivery, non-interrupting wake-on-idle intent,
claim/dispatch/reconciliation state, and optional reply rounds; runtime adapters
decide how to notify, resume, or activate their native agents.

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
  -den-mcp-url "${DEN_MCP_URL:-http://192.168.1.10:5199/mcp}" \
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

Before admitting a reviewer, crew-review checks read-only (`git merge-base
--is-ancestor`) that the resolved checkout contains the submitted commit. If it
does not yet, usually because the checkout has not been pulled, the submission
waits quietly in `source_pending`; after `-source-grace` (15 minutes by default)
it stops as `source_missing` with `error_code: "checkout_missing_commit"`. This
never produces a review verdict or a message to the submitter, and crew-review
never fetches or checks out. The admitted job carries the submitted
`base_commit..commit_sha` range, which the reviewer prompt names explicitly.

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

## Project selected Codex threads

`crew-codex` is a separate runtime adapter command. It supervises the ordinary
local `codex app-server --stdio` child, reads explicitly selected existing
threads, projects their canonical history into the session/event surface, and
accepts ordinary fabric deliveries through Codex's native FIFO queue. It does
not steer, interrupt, resume, or alter native approval and tool handling.

Start the fabric first, then run one explicit address-to-thread mapping for
each Codex thread that should appear in the directory:

```sh
go run ./cmd/crew-codex \
  -fabric-url http://127.0.0.1:8787 \
  -address crew/scout=YOUR_CODEX_THREAD_ID
```

Mappings are repeatable. A native thread may occur only once, and an occupied
address owned by another adapter is rejected rather than rebound. The native
thread ID is held in the adapter-private session key; the public binding points
to the fabric-owned `session_id` instead. A session's current label, working
directory, and runtime status are CAS-updated from `thread/read`; changing the
display name never changes its identity.

The default `-instance-id crew-codex-local` is intentionally stable so a normal
process restart renews the same adapter lease. Choose a distinct stable value
only when running a second independent adapter instance against the same
fabric.

The adapter rereads canonical `thread/read { includeTurns: true }` history on
each polling pass and after an App Server restart. It projects completed
user/agent message entries with stable native entry/turn IDs and stable fabric
operation IDs, so a full replay is idempotent. It intentionally leaves native
notifications, partial agent messages, reasoning, tool activity, files, and
approval requests out of this first durable event projection. Those are later
adapter/UI slices, not an implied generic transcript format.

`crew-codex -state PATH` stores a small adapter-owned JSON receipt after a
successful browser-created native thread: the create operation ID, native
thread mapping, and fabric public session ID. Reusing that path lets a normal
adapter restart replay a successful create and rejoin projection of the thread.
It does not persist pending App Server callbacks; approvals and input requests
vanish if the native child exits and must be reissued by Codex.

Threads created through the crew-codex control API start with `crew_directory`
and `crew_message` dynamic tools. Existing explicit `-address` mappings remain
projection-only because Codex cannot retrofit dynamic tools onto a resumed
thread. The catalog may still be visible after an address is unbound, but each
call re-resolves the current fabric binding and fails closed; no native thread,
lease token, or fabric target reference is returned to Codex.

For a session advertising `queued-prompt-delivery`, the DSH workbench may send
an ordinary fabric message to its mapped address. `crew-codex` claims the exact
binding generation and begins one fabric dispatch. A newly created, still
unmaterialized control thread uses `turn/start` once with a delivery-derived
`clientUserMessageId`; a materialized thread uses `thread/queue/add` and Codex
owns the later FIFO start once it is idle. An active turn is never steered or
interrupted. If the adapter loses either native response, it rereads canonical
thread history (and, for queue admission, the native queue) for that client ID
before it acknowledges or records `outcome_unknown`; it never sends the prompt
again.
