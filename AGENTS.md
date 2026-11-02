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

## crew-board (design: `docs/crew-board.md`)

`crew-board` is a separate product in this repository: a durable discussion
board whose threads stand on their own and notify participants through
crew-messaging as an ordinary producer (`system/crew-board`). It is its own
command (`cmd/crew-board`), its own `internal/board/...` packages, its own
SQLite file and its own unit, and it follows the same handler, service and
store split as the hub. It reaches the fabric only over HTTP through
`fabricclient`. It never imports the messaging `service`, `store`, `sqlite` or
`httpapi` packages, nothing in the hub imports it, and an import test enforces
both directions. Board, thread, post and site concepts never enter the fabric.

The boundary rule above is for the fabric. Patch approved these crew-board
requirements on 2026-11-02:

- **Media attachments:** images and short moving images only, transformed on
  ingest and checked on every site.
- **Shared boards:** a few known sites share boards through a private git
  repository. The only network traffic is outbound git over SSH, with git's SSH
  commit signing and trust pinned in each site's own config.

crew-board's own HTTP API stays on loopback. Patch approved one wider
listener on 2026-11-02: the people's web pages (`-web-listen`, port 8795)
may listen on the LAN, and phones reach them over Tailscale. They answer only
private, Tailscale and loopback addresses, and nothing wider; the Caddy site
`board.dragonden.stream` that fronts them makes the same check. They have no
login, so the network is the trust boundary; they are styled for dark mode
only. Any further exposure needs its own approval.

## Verification and repository care

Use proportionate proof: focused unit/state-machine tests plus the documented
selection suite, restart checks, and adapter-visible evidence where behavior
crosses the native runtime boundary. Keep accepted, native insertion, processed,
read, and replied as separate facts. Never let inspection/replay/history cause
claiming, waking, retrying, cancelling, or delivery.

Treat waking, steering and interrupting as separate concepts. The sender picks
an activation policy: `next_turn` (the default) never touches an active turn,
`next_step` may put the message in front of an active turn at its next step
boundary, and `no_wake` never starts a turn. Steering is only ever that
sender-chosen, non-cancelling `next_step` delivery; nothing in the fabric
cancels or interrupts a turn. Drivers post into each runtime's own inbox and
own idle detection; they do not supervise runtime processes. The fabric owns
the durable policy and pending delivery. See `docs/messaging-redesign.md`.

This repository may be edited concurrently. Preserve unrelated work, inspect
status before modifying files, do not reset/clean/revert others' changes, and do
not create remote repositories, commits, or unrelated scaffolding without
explicit instruction.
