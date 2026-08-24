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

## Project selected Codex threads

`crew-codex` is a separate runtime adapter command. It supervises the ordinary
local `codex app-server --stdio` child, reads explicitly selected existing
threads, and projects only their canonical history into the session/event
surface. It does not start turns, inject prompts, respond to native approvals,
or install dynamic tools.

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
