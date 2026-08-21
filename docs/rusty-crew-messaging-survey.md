# Rusty Crew messaging survey

## Scope

This survey records direct observations from `/home/dev/rusty-crew` at
`3b37de5e7a432b6d477c83f337e5bbbc105358cb`. The identifier describes evidence
only; it is not a pin, compatibility promise, or reason to freeze upstream.
Rusty Crew is a donor of behavior and failure lessons, not structure.

## Two message planes

Rusty Crew has a legacy **in-process event plane** and a newer **durable
coordination plane**.

- `BrainAction::SendMessage` validates exact sender/target sessions and publishes
  `AgentMessageRouted`, but creates no delivery receipt, TTL, idempotency record,
  or wake request (`crates/core/core-body/src/lib.rs:137-195`).
- `CoreBus::route_message` builds an `AgentMessage`, while
  `route_agent_message` publishes `AgentMessageRouted` only
  (`crates/core/core-bus/src/lib.rs:113-133`). It records before fanout,
  retains bounded in-memory history, then sends only to current subscribers
  (`crates/core/core-bus/src/lib.rs:135-162`). Legacy session lookup can accept
  an agent-only destination, ambiguous when sessions share an agent identity
  (`crates/core/core-bus/src/lib.rs:187-212`).
- `CoreEngine::deliver_agent_message` builds an identity-bound request and
  persists a pending receipt before routing or activation
  (`crates/core/core-engine/src/agent_coordination.rs:454-545`). The protocol
  exposes request, status, activation, reason, terminal time, and revision
  (`crates/core/core-protocol/src/external_runtime.rs:1202-1224`,
  `crates/core/core-protocol/src/external_runtime.rs:1371-1390`). TS exposes
  it through tools, operator routes, and Codex dynamic tools
  (`ts/packages/brain-island/src/coordination-tools.ts:17-105`,
  `ts/packages/brain-island/src/service-coordination-operator-routes.ts:68-256`,
  `ts/packages/external-runtime-codex/src/coordination.ts:11-89`).

The successor needs one explicit durable fabric contract; it must never infer
durability from an event name or UI route.

## Identity, directory, routes, and provenance

Crew offers raw `agent_id` addressing plus curated `@route` addresses. A route
target is direct brain `(agent_id, session_id)` or managed external
`(agent_id, binding_id, binding_revision)`; the record carries route revision
and runtime/delivery constraints
(`crates/core/core-protocol/src/external_runtime.rs:82-179`). The engine
reserves route-looking raw IDs and resolves raw names through an agent directory
(`crates/core/core-engine/src/agent_coordination.rs:21-130`). Curated routes
are exact; raw agents retain ambiguity.

The durable delivery captures requested address, resolved agent/session, optional
route provenance, reply link, caller input, correlation, wake option, timestamps,
and idempotency key (`crates/core/core-protocol/src/external_runtime.rs:1202-1224`).
Commands separately model the caller as system, direct brain, external
agent/controller/native-thread, CLI, or review submission
(`crates/core/core-protocol/src/external_runtime.rs:1301-1325`). Those details
are validated at command entry, but the durable request retains only the
logical/concrete sender and not the full caller record. A successor should
choose durable producer/audit provenance deliberately rather than assuming
validation made it persistent.

**Successor lesson:** preserve exact resolution, provenance, expected-revision
writes, and binding snapshots. Replace runtime terms with
`Address -> Binding(adapter_id, opaque target_ref, generation, capabilities)`.
The fabric owns stable addresses; the adapter owns native references.

## Durable envelope, TTL, and acceptance

Crew creates a pending receipt through the store before checking expiry,
routability, events, or activation; idempotent creation returns the existing
receipt (`crates/core/core-engine/src/agent_coordination.rs:508-545`). Dead
messages expire and unmapped/archived recipients receive reason-coded rejection
(`crates/core/core-engine/src/agent_coordination.rs:545-679`).

Statuses are `Pending`, `Accepted`, `Rejected`, and `Expired`
(`crates/core/core-protocol/src/external_runtime.rs:1180-1197`). With
`require_wake: false`, Crew publishes and accepts without activation
(`crates/core/core-engine/src/agent_coordination.rs:681-694`). External turns
have a separate lifecycle beginning at Accepted
(`crates/core/core-protocol/src/external_runtime.rs:903-946`).

**Failure lesson:** accepted cannot mean model processed, read, or completed.
The successor must distinguish immutable `operation_id` from `message_id`:
exact API retry returns the original result, while reuse with different content
conflicts. It makes no exactly-once processing claim. Native insertion can be
ambiguous; adapters reconcile via `delivery_id` and native receipts, recording
`outcome_unknown` rather than pretending a post-side-effect lease expiry is
always safe to redeliver.

## Activation, non-interruption, and serial queues

For wake-required work, Crew frames model text, derives activation/capacity IDs,
attaches provenance, and selects target activation
(`crates/core/core-engine/src/agent_coordination.rs:697-732`). Results include
direct wake, new external turn, immediate steer, queued-next-turn, and rejection
(`crates/core/core-protocol/src/external_runtime.rs:1011-1035`); the external
path selects among them from target/runtime activity
(`crates/core/core-engine/src/external_runtime.rs:710-870`).

Queued routed messages reuse the body follow-up queue and enforce serial inbox
capacity (`crates/core/core-engine/src/body.rs:37-105`). Wake preparation drains
pending items into body state (`crates/core/core-engine/src/body.rs:12-24`), and
the queue helper may discard older entries for the history window
(`crates/core/core-engine/src/body_queue.rs:47-105`). The generic table holds
owner agent/session, body, correlation, TTL, attempts, state, and reason
(`crates/core/core-persistence/src/repos/queued_messages.rs:8-31`).

Keep serial delivery and native activation selection, not this shared topology.
The successor owns one delivery queue and records activation intent, while the
adapter owns the runtime-specific wake mechanism. The default intent is
`wake_when_idle`: never steer, cancel, or inject the full message into an active
turn; optionally surface a pending notification or durable next-turn follow-up;
then wake/resume and deliver when the target reaches an idle boundary. An
optional `never_wake` intent is opportunistic only and remains queued while the
target is inactive. Lease/fencing coordinates work; it is not authentication or
a security scheme. Crew's `ImmediateSteer` is useful donor evidence for an
explicit future interrupting policy, not the successor's default message path.

## CoreBus is not durable wake

The bus records before fanout, but fanout is only to current in-process
subscribers (`crates/core/core-bus/src/lib.rs:135-162`). Direct wake publication
occurs after receipt persistence and activation selection
(`crates/core/core-engine/src/agent_coordination.rs:733-758`). A crash between
persistence and TypeScript dispatch can lose the wake while the record survives;
history replay must not actuate it.

**Successor fix:** delivery is executable work:
`queued -> claimed -> dispatching -> delivered`. A pre-dispatch claim may return
to queued on a safe release/expiry. Once an adapter durably records dispatch
start, lease loss requires native reconciliation and can become
`outcome_unknown`; it cannot blindly return to the queue. Claims use token,
lease, attempts, binding-generation snapshot, native attempt reference, and
terminal reason. This is not an exactly-once guarantee. Inspection, list,
history, and replay are read-only.

## External runtime: recovery donor

Crew persists controller leases, bindings, controls, receipts, interactions,
external events, and correlated rounds separately
(`crates/core/core-persistence/src/repos/external_runtime.rs:25-138`).
External controls fence side effects with expected binding revision and optional
native turn (`crates/core/core-protocol/src/external_runtime.rs:1052-1077`);
runtime code reaps expired rounds
(`crates/core/core-engine/src/external_runtime.rs:900-980`).

The strong donor pattern is atomic queue/provider promotion, restart
reconciliation, fencing, and binding revision. Adapt it as fabric-native
claim/ack/release plus semantic binding generation. Do not import Crew's
controller, profile, native turn, runtime, or schema topology.

## Replies and correlated rounds

Crew's ordinary reply accepts the original message ID and resolves
sender/correlation; its tool describes reply-once
(`ts/packages/brain-island/src/coordination-tools.ts:151-176`). The request
stores `reply_to_message_id`
(`crates/core/core-protocol/src/external_runtime.rs:1202-1237`).

`begin_agent_round` first persists a pending round, then creates a
wake-required delivery (`crates/core/core-engine/src/agent_coordination.rs:786-849`).
Rounds store endpoints, initiating message, correlation, reply ID, outcome,
timestamps, and revision (`crates/core/core-protocol/src/external_runtime.rs:1394-1411`).
A pending round expires on a later read
(`crates/core/core-engine/src/agent_coordination.rs:852-872`).

The round uniqueness key is sender agent, recipient agent, and correlation
(`crates/core/core-persistence/src/repos/external_runtime.rs:161-180`).
It is agent-only rather than exact route/binding/message matching, so concurrent
sessions can collide. Also, round creation precedes the delivery call, allowing
an orphan-pending-round failure gap if delivery creation fails.

**Successor decision:** ordinary linked replies are not globally limited to one.
Each is a new exact-linked message. A Round resolves exactly once, on its first
valid reverse reply. Create the Round plus root message and root delivery in one
transaction, then use exact address/message linkage (correlation is diagnostic)
to eliminate both the collision and orphan gap.

## Persistence split, reaping, and ordering

Crew state spans `queued_messages`
(`crates/core/core-persistence/src/repos/queued_messages.rs:8-31`), delivery
receipts and rounds (`crates/core/core-persistence/src/repos/external_runtime.rs:146-180`),
external turns/events/bindings/leases
(`crates/core/core-persistence/src/repos/external_runtime.rs:25-138`),
revisioned routes (`crates/core/core-persistence/src/repos/agent_routes.rs:6-25`),
and in-memory event history. Do not clone it.

The generic queue upsert rewrites queued record fields
(`crates/core/core-persistence/src/repos/queued_messages.rs:37-100`), its
reaper scans pending entries (`crates/core/core-persistence/src/repos/queued_messages.rs:102-140`),
and terminal retention is separate
(`crates/core/core-persistence/src/repos/queued_messages.rs:142-167`).
This leaves ambiguity among body queue, external turn, receipt, and event state.

**Successor decision:** one fabric retention/reaping policy owns immutable
messages, mutable deliveries, bindings, and rounds. FIFO is per recipient
address and accepted binding generation: the oldest nonterminal eligible
delivery is the head; later deliveries are not claimable while that head is
queued or safely claimed. A head that is expired, cancelled, terminally failed,
or reconciled `outcome_unknown` is terminalized before the next may proceed.
This is intentional head-of-line blocking, not a loose best-effort ordering
claim.

## Higher workflows do not transfer

Completion packets and managed Den review are higher workflows, not generic
messaging. A `CompletionPacket` contains a session, status, and summary but no
message identity, address, TTL, delivery, or reply semantics
(`crates/core/core-protocol/src/types.rs:934-940`); `DeliverCompletion` publishes
it as its own event (`crates/core/core-body/src/lib.rs:143-145`). Crew places
review tools beside coordination
(`ts/packages/external-runtime-codex/src/coordination.ts:90-190`) and removes
the low-level reply tool from managed reviewers
(`ts/packages/external-runtime-codex/src/coordination.ts:194-218`). Their
Den/GitHub/finalization semantics do not belong in the fabric.

## Preserve, adapt, reject

| Preserve | Adapt | Reject |
| --- | --- | --- |
| Idempotent TTL-bound acceptance; provenance; reason codes; binding revisions; serial queues; linked replies; recovery evidence. | Address-to-opaque-binding; leased claims; generation snapshots; exact round matching; delivered only after native acceptance; outcome-unknown reconciliation. | Event-only delivery; raw-agent ambiguity; direct brain/external turn concepts in core; shared body queue authority; review/completion workflows; donor schemas/crates. |

The successor is smaller because the fabric owns delivery execution and recovery.
Adapters translate a claimed delivery into native pending notification,
next-turn insertion, or wake/resume activation.
