# AGENTS.md

## Scope and posture

`crew-services` is a runtime-neutral successor, not a Rusty Crew extraction.
Preserve required behavior, contracts, tests, and failure lessons from donors;
do not reproduce their crate layout, schemas, product vocabulary, workflow
machinery, or accidental implementation shape. Current donor revisions can
explain an observation but are never compatibility pins or acceptance gates.

The core service must not contain DSH, Rusty Crew, Codex, model, provider,
profile, workspace, native-session, or UI concepts. Those belong behind
adapters. The core owns stable addresses, bindings and their generations,
immutable messages, producer provenance, claimed/dispatching delivery state,
idempotency, TTL, ordering, durable wake claims, reply/round state, diagnostics,
retention, and reason codes.

## Moving upstreams and donors

When work relies on DeepSeek Harness, inspect its checkout, preserve local
work, and pull current fast-forward upstream before adapting the relevant
adapter surface. Do not turn normal DSH movement into pins, SHA gates, lock
procedures, or freeze machinery. Refresh any available codebase map after a
meaningful donor update, then read exact current source before making
parser-sensitive claims.

Consult donors before inventing a behavior that they already demonstrate:

- Rusty Crew is behavioral evidence for messaging and runtime activation.
- Den services donates small typed Handler -> Service -> Store boundaries,
  constructor injection, clocks, immutable records, receipt separation, and
  atomic claim/idempotency patterns.
- ANP donates envelope layering, logical-versus-transport identity, and the
  `operation_id`/`message_id` distinction; it is a future gateway, not v1.
- dsh-awiki donates provider/service/consumer separation, canonical sync,
  durable routes/watermarks, and per-conversation serialization/idempotency.

Do not import donor product/task/review vocabularies, Postgres roles,
deployment/review ceremony, AWiki runtime/identity, or an AGPL core dependency.

## Implementation conventions (when implementation begins)

Build one Go module. Keep the runtime-neutral hub in the `crew-messaging`
binary; runtime-specific adapters may be separate commands in this repository,
but core packages must never import them. Keep the HTTP handler thin, put
contract and state-machine decisions in a service package, and isolate SQLite
behind a store interface. Inject the store, clock, and adapter-facing interfaces
through constructors. Use typed configuration only: loopback listen address,
SQLite path, and lease/TTL/retention bounds. Prefer ordinary JSON/HTTP and SQLite
transactions over platform abstractions or a plugin/UI layer.

The service is a trusted-box loopback process for v1. Do not add public-web
auth, crypto, federation, Postgres, multi-module layouts, groups, streaming,
attachments, or broad enterprise hardening unless a later approved requirement
changes the boundary.

## Verification and repository care

Use proportionate proof: focused unit/state-machine tests plus the documented
selection suite, restart checks, and adapter-visible evidence where behavior
crosses the native runtime boundary. Keep accepted, native insertion, processed,
read, and replied as separate facts. Never let inspection/replay/history cause
claiming, waking, retrying, cancelling, or delivery.

Treat wake intent and interruption as separate concepts. The default messaging
policy is non-interrupting wake-on-idle: adapters may advertise pending work to
a busy runtime, but must not steer, cancel, or inject the full message into its
active turn. Runtime-specific adapters own activation and idle detection; the
fabric owns the durable policy and pending delivery.

This repository may be edited concurrently. Preserve unrelated work, inspect
status before modifying files, do not reset/clean/revert others' changes, and do
not create remote repositories, commits, or unrelated scaffolding without
explicit instruction.
