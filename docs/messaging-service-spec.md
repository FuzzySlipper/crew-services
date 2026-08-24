# Messaging service specification

## Status and intent

This is a proposed v1 contract, not an implementation claim. `crew-services`
will be an independent, runtime-neutral local messaging fabric. The intended
first implementation is one Go module, one binary, SQLite, and JSON/HTTP on a
loopback listener. DSH, Rusty Crew, Codex app-server, and future transports
participate only through adapters.

The design preserves useful cross-session behavior observed in Rusty Crew while
rejecting its incidental topology. An observed donor snapshot may aid research;
it is not a compatibility target or release gate.

## Boundary and vocabulary

The fabric owns stable logical addresses; adapter registration/leases; bindings,
their revisions and semantic generations; immutable messages; mutable delivery
attempts/leases; idempotency; TTL; FIFO ordering; durable wake claims; replies
and rounds; producer provenance; reason codes; diagnostics; and retention.

An adapter owns mapping an opaque binding `target_ref` to its native runtime's
session/thread lifecycle. It performs native activation, resume, pending-work
notification, or safe next-turn insertion; owns native transcript,
model/provider/profile/workspace details; and acknowledges delivery only after
its runtime accepts or inserts the message.

```
Address
  -> Binding(adapter_id, opaque target_ref, generation, capabilities)
  -> native runtime target
```

The address is fabric-owned. `target_ref` is opaque to the fabric. A native
session or thread is never an address and must not appear in core semantics.
The service never calls DSH, Rusty Crew, or Codex directly.

## Core records

### Immutable message envelope

Each accepted message stores one immutable envelope: `operation_id`
(retry/idempotency identity), `message_id` (logical identity), `producer_id`,
sender and recipient address, v1 text body, `correlation_id`, optional
`reply_to_message_id`, `activation_policy`, `created_at`, `expires_at`, and the
sender/recipient binding-generation snapshots.

V1 defines `wake_when_idle` as the default activation policy. It means “deliver
without interrupting current work, then activate when a safe idle boundary is
available,” not “steer the active turn.” An optional `never_wake` policy supports
opportunistic delivery only; it is not the baseline collaboration behavior.

`operation_id` and `message_id` have different jobs. Exact retries return the
original result; idempotency is scoped by stable `producer_id`, and reusing its
operation ID with a different request conflicts. An adapter lease proves only
that the current instance may act for that producer; its ephemeral lease token
is not part of the immutable request fingerprint. A reply is a new envelope
with exact `reply_to_message_id`, never a mutation of the original.

### Binding, lease, and generation

Adapters register and renew a lease. A binding maps an address to an adapter,
opaque target reference, capabilities, and semantic generation. Binding writes
use expected revision. Acceptance snapshots the binding generation. A semantic
rebind makes old queued work fail with a reason instead of silently misrouting.
Adapter restart, lease renewal, and claim renewal do not increment the semantic
generation; only intentional target change does.

An adapter submission may name only a sender address currently bound to that
adapter. This is coordination provenance and fencing on a trusted box, not an
authentication claim. Any future operator/system ingress uses an explicit
service-owned origin instead of impersonating an agent address.

### Session projection and event log

The same local database holds a bounded, runtime-neutral projection for clients
that need to discover adapter-owned work. A session has a stable service ID,
adapter diagnostic ID, label, location, status, capabilities, revision, and
timestamps. Adoption is idempotent on `(adapter_id, opaque_adapter_key)`: an
adapter restart returns the original public session ID rather than creating
another one. The opaque key stays persistence-only and is never returned in
client JSON.

An adapter with a live lease owns its projected sessions. It may CAS-update a
session at its current revision and append named JSON facts at that revision.
Events have a per-session sequence plus a global cursor, event ID, event type,
payload, occurred time, and recorded time. They are projection/read-model facts,
not an authoritative native transcript, runtime lifecycle, or model context.
Exact adapter-scoped `operation_id` retry returns the original event even after
later session-revision or lease drift; the revision fences only a first append.
Reuse for a different session, type, or payload conflicts.
When supplied, the occurrence time is part of that immutable event identity;
omitting it keeps retries independent of the service-recorded current time.

Reads are bounded and side-effect free: list/get session, event history after a
cursor, and SSE replay/resume only observe the durable log. SSE accepts a query
cursor or `Last-Event-ID`, can filter one public session ID, replays in global
cursor order, then follows new events using local polling. It never claims,
wakes, inserts, retries, or changes runtime work.

### Delivery state machine

A delivery is mutable executable work separate from its message:

```
queued -> claimed -> dispatching -> delivered
claimed -> queued                 (release or lease expiry before dispatch)
queued | claimed -> failed | expired | cancelled
dispatching -> delivered | failed | outcome_unknown
```

A claim carries a claim token, lease expiry, attempt count, and terminal reason.
Before making a native side effect, the adapter moves the claim to
`dispatching` and records its stable native attempt/idempotency reference. A
plain `claimed` lease can safely return to `queued`; a lost `dispatching` lease
cannot. It remains fenced for adapter reconciliation and becomes
`outcome_unknown` if the native result cannot be established. Terminal
deliveries are immutable. `accepted` means envelope plus queued work was stored,
never model processed/read/completed. `delivered` means the adapter reports
native acceptance/insertion. Processed, read, and replied are separate facts.

Only explicit adapter claiming actuates work. Listing, resolving, mailbox
inspection, history, and replay never create, claim, wake, retry, or cancel
delivery work. `wake_when_idle` never cancels, steers, or injects the message
into a busy turn. While the target is busy, the adapter may surface a
runtime-specific pending notification or register a durable next-turn follow-up.
If it cannot do that durably, the fabric delivery remains queued. When the
target reaches an idle boundary—or is already inactive—the adapter may
resume/activate it and deliver the message as the next turn.

`never_wake` may insert only at an adapter-defined safe boundary in an already
active target; otherwise it remains queued. A crashed adapter's pre-dispatch
claim safely expires back to the queue; a dispatch already begun must be
reconciled. Exact duplicate API retries never create a second delivery or wake.

This is API idempotency and leased delivery, not exactly-once processing. An
adapter uses `delivery_id` plus a native receipt to reconcile ambiguous native
insertion. If it cannot determine whether the native side effect happened, it
records `outcome_unknown`; it must not blindly redeliver after a lease expires.
Lease and fencing coordinate adapters, not authentication or a security scheme.

### Replies and rounds

Ordinary replies are new, exactly linked messages back to the original sender
address and are not globally limited to one. An optional durable request/reply
round records the initiating message and exact endpoint addresses/message IDs:

```
pending -> replied | expired | cancelled | failed
```

The first valid reverse reply resolves a Round exactly once. Correlation is
helpful metadata, not the sole join key: matching is exact-address and
exact-message based. Creation of a Round, its root message, and root delivery
is one transaction, so a failed send cannot leave an orphan pending Round.

## API direction

Paths are provisional; these operations are semantic commitments:

| Operation | Semantics |
| --- | --- |
| register / renew adapter | Create or renew an adapter lease and capabilities. |
| put / update binding | Create or CAS-update an address binding; semantic target change advances generation. |
| list / resolve addresses | Read bindings/routability without side effects. |
| submit message | Fence the producer and sender binding, resolve the recipient, snapshot both generations, then atomically persist envelope and queued delivery with producer-scoped idempotency. |
| claim deliveries | Adapter-only, optionally long-polling; atomically claim eligible FIFO work with token and lease. |
| begin dispatch | Token-fenced transition recorded immediately before the native side effect, including its stable attempt reference. |
| acknowledge / release / fail / outcome unknown | Token-fenced adapter outcomes; acknowledgement means native acceptance only, and ambiguity is preserved for reconciliation. |
| inspect message / delivery / mailbox | Read-only inspection and history. |
| adopt / update session | Adapter-owned, lease- and revision-fenced runtime-neutral projection. |
| append / replay session event | Append projection fact with adapter operation idempotency; replay from global cursor or SSE. |
| begin / get / wait / cancel round | Durable request/reply lifecycle; waiting is observational. |

V1 is a trusted local box: loopback only, no authentication or encryption.
Typed configuration is limited to listen address, SQLite path, and
lease/TTL/retention bounds.

## Storage, transactions, and proving slice

SQLite owns fabric state. Messages are append-only; delivery attempts and facts
are separate rows. Transactions must make submit/idempotency, round-root
creation, claim/fencing, dispatch start, acknowledgement, release, and reaping
atomic. Store a request fingerprint to distinguish exact retry from
operation-ID conflict.

FIFO is per recipient address and accepted binding generation. The oldest
eligible nonterminal delivery is the head; later deliveries cannot be claimed
while that head is queued, claimed, or dispatching. A head that is expired,
cancelled, terminally failed, or `outcome_unknown` is terminalized before the
next becomes eligible. This intentional head-of-line rule makes FIFO meaningful.
A periodic reaper expires TTL-bound work, releases only pre-dispatch stale
claims, and terminalizes unreconciled dispatches as `outcome_unknown` after the
adapter lease is gone. It also applies bounded terminal-record retention.

The first selection suite proves:

1. Two unrelated DSH root sessions bind two addresses.
2. Idempotent send creates one message and queued delivery.
3. `wake_when_idle` sent to a busy target never steers, cancels, or mutates its active turn.
4. A capable adapter surfaces a pending notification or durable next-turn follow-up while busy; otherwise the fabric keeps the delivery queued.
5. At the idle boundary, the adapter wakes/resumes the target and FIFO-drains eligible work.
6. Optional `never_wake` work remains queued while the target is inactive.
7. Delivered occurs only after native acceptance of the message or durable next-turn insertion, never from a notification alone.
8. A correlated reply can schedule the original sender under the same non-interrupting policy.
9. Restart retains queue and bindings.
10. Adapter crash before dispatch safely releases; a crash during dispatch reconciles a native receipt and never blindly redelivers an ambiguous insertion.
11. Semantic binding change fails old queued work.
12. Duplicate retry never duplicates wake.

Unit tests cover state machines, CAS/fencing, TTL, conflict fingerprints, and
read-side non-actuation. The DSH slice needs adapter-visible or live evidence
for native activation/insertion; service-only tests cannot prove that boundary.

## Non-goals and donor comparison

V1 excludes attachments, groups, streaming, public federation, public-web
crypto, authentication frameworks, Postgres, multi-module/platform layers,
plugin work, and UI work. Future work may add Crew/Codex adapters, a DSH-Crew
roundtrip, and an ANP gateway without changing core ownership.

| Donor | Preserve/adapt | Reject |
| --- | --- | --- |
| Rusty Crew | Durable identity-bound deliveries, provenance, TTL, queueing, route revisions, native activation outcomes, reply links. | Crates, direct-brain/runtime concepts, bus topology, mixed queues, managed review, schema history. |
| Den services | Typed Handler -> Service -> Store, constructor injection/clock, immutable messages plus separate receipts, atomic claims/idempotency. Evidence: `messages/internal/types.go:88-154`, `messages/internal/service.go:18-54`, `delivery/internal/store.go:24-145` at observational `3094e0279a62739dd934411ba6dd010ded7561bb`. | Task/project/review vocabulary, Postgres roles, deployment/review ceremony. |
| ANP | Envelope layering, logical-versus-transport identity, and `operation_id`/`message_id`. | Treating a network protocol as local wake/queue/receipt solution; DID/TLS/federation/E2EE are future-gateway concerns. |
| dsh-awiki | Provider/service/consumer split, canonical sync, durable routes/watermarks, per-conversation serialization/idempotency. | AWiki identity/runtime and an AGPL core dependency. |

Den's extraction rule is directly applicable: replayed records must not create,
claim, wake, retry, complete, or cancel executable work
(`docs/messages-lifeboat-contract.md:27-44`).
