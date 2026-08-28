# crew-services

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
`/home/agents/profiles/reviewer/SOUL.md`, and sends only the structured
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
  -review-profile "${CREW_REVIEW_PROFILE:-/home/agents/profiles/reviewer/SOUL.md}" \
  -capacity 2
```

`DEN_MCP_TOKEN`, `CREW_REVIEW_PROFILE`, `CREW_REVIEW_CAPACITY`,
`CREW_REVIEW_RUN_INTERVAL`, and `CODEX_COMMAND` may be supplied through the
environment; all have corresponding flags where they affect the process.
The command starts a fixed number of bounded runner lanes (one durable job per
lane at a time), reports `backend: "codex"` from `GET /v1/review-pool`, and
never exposes ephemeral Codex worker or thread IDs in that projection.

The managed submission boundary is `POST /v1/review-submissions`. The Den MCP
facade routes its `submit_task_for_review` green path to this endpoint through
the separately configured `crew-review` backend. A first call records the Den
round and exact-SHA gate; it returns `phase: "gate_pending"` while checks are
pending, and later retries of the same target advance to `phase:
"job_admitted"` once Den reports the gate passed and the current review context
is source-review-ready. Submission state and round-scoped job admission are
durable in the local SQLite file, so an uncertain retry reconciles instead of
starting a second job. An unavailable crew-review backend is returned as an
actionable retryable result; there is no automatic Rusty fallback.

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
