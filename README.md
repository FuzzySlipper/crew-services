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
