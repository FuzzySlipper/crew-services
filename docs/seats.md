# Seats

Status: built (2026-10-31, Den task #10519). The fabric core is schema
migration 10, the store and service operations, the HTTP routes under
`/v1/seats`, and the proving-slice tests in `internal/sqlite/seat_test.go`. The
drivers (crew-claude, crew-codex, crew-dsh) claim seat mail and share the
`seat` commands and `send --as`. A live Claude Code and Codex run is recorded
in [Seats across harnesses](evidence/seats-cross-harness.md). This note is the
seat part of the [messaging service specification](messaging-service-spec.md),
which summarizes its core rules, and it keeps the decisions and the reasons
behind them.

## Why

Roundtables and longer cross-session discussions need a stable "who": an
address that keeps meaning the same thing while the sessions behind it come
and go. A session address (`s/<hash>`) names one native session. `claude@box`
or `claude@project` names a harness or a place, and neither says what the
session is answering for. Profile layers in the style of Hermes `soul.md` add
per-agent machinery and depend on a particular harness.

A **seat** is a position that a session holds for a while: an address, a
charter of duties, and a handoff note from the last holder. It is separate
from the session, the harness and the machine. A session takes a seat, answers
for it while holding it, and leaves or is relieved with a note. A convention in
a skill alone is not enough, because two sessions could each believe they hold
the same seat. The fabric enforces that one session holds a seat at a time, and
the same record answers "who holds this seat right now".

### Why "seat" and not "role"

The name shapes how agents use it. "Role" pulls the wrong way three times over:
in model training data "your role is X" is the standard persona prompt, so an
agent given a role tends to change its voice rather than take on
accountability; in access control a role grants permissions, which seats
do not; and this stack already uses `role` for the chat API's
`"role":"user"` field (`docs/messaging-redesign.md`). "Seat" implies one
occupant ("this seat is taken"), has a natural vacant state, allows one person
to hold several seats, fits a roundtable literally, and carries no persona or
authorization meaning. The verbs are `take`, `leave` and `relieve`; "relieve"
comes from watch-standing, where it is a cooperative handover, not a hostile
"unseat".

## What the fabric owns, and what it does not

The fabric owns seat records, holdings, the charter and handoff texts with
their bounded version history, seat-addressed delivery, and seat provenance on
messages. A seat is runtime-neutral: no harness, model, profile or Den concept
appears in its record.

Seats are not an authentication or permission system. Holding and fencing
coordinate sessions on a trusted box, exactly as adapter leases do. Seats do
not dispatch or assign work: Den stays the only task record, and a seat routes
conversation and shows who answers for something.

## Seat address

A seat address has the form `name@scope`. Both parts are lowercase letters,
digits and hyphens. The `@` form is reserved for seats and is never used for
another kind of address.

The scope is an opaque namespace. The fabric does not know what a project or a
roundtable is and enforces no scoping rules: `lead@rusty-engine`,
`reviewer@crew-services` and `skeptic@rt-12` are just three seats. There are no
global seats without a scope, and there is no hierarchy between scopes.

Names may be functional or playful. A playful name works as a prompt (a
`skeptic` seat will tend to argue), which is often what a roundtable wants;
names should be chosen with that in mind, and the charter carries the actual
duties.

## Records

### Seat

| Field | Meaning |
| --- | --- |
| `address` | `name@scope`. |
| `generation` | Advances only when the seat is retired and created again. A change of holder never advances it. |
| `created_at`, `created_by` | First write that created the seat (see *Creating a seat*). |
| `holding` | The current holding, or none when the seat is vacant. |
| `charter_version`, `handoff_version` | Current version numbers of the two texts (0 = never set). |

### Holding

One holding is one session's time in one seat. A holding is never edited
after it ends.

| Field | Meaning |
| --- | --- |
| `holding_id` | Increases per seat across holders; it fences sending as the seat and claiming seat mail. |
| `holder_address` | The holder's session address (`s/<hash>`). |
| `holder_adapter_id` | The adapter that owned that session binding when the seat was taken. |
| `holder_generation` | The session binding's generation when the seat was taken. |
| `started_at` | When the seat was taken. |
| `ended_at`, `end_reason` | `left`, `left_without_handoff`, `relieved`, or `retired`. |
| `relieved_by` | The holding that replaced it, for `relieved`. |
| `note` | Optional free text given with take, leave or relieve. |

A session may hold any number of seats at once. A session that is both
`reviewer@crew-services` and `lead@crew-services` holds two holdings.

### Charter and handoff texts

Each seat has exactly two texts: one **charter** (duties and concerns) and one
**handoff** (the note for whoever holds the seat next). There is no way to
create a second charter, a second handoff, or any other kind of seat text.

- **Set, never append.** Writing a text replaces it completely and creates a
  new version. An agent that wants to carry text forward reads the current
  version and sets the current text plus its additions. No other tool exists
  for that, and none should be added: whether a handoff stays a terse list of
  current concerns or grows into a journal with a carry-forward structure is
  left to whoever writes it.
- **No length limit.** The stored text has no size bound. Limits, if they are
  ever needed, belong on the surfaces that put text into an agent's context
  (see *Driver and CLI surface*), not on storage.
- **Versions.** Version numbers increase by one per seat and kind and are never
  reused. Each version records its text, `set_at`, the setting session's
  address and adapter, the seats that session held at that moment, and the
  holding (if any) it was set during. The fabric keeps the newest N versions
  per seat and kind and prunes the oldest in the same transaction as the write
  that pushes it out. N is typed configuration (`seat_text_versions`, default
  50). Asking for a pruned version returns `version_pruned`, which is distinct
  from `version_not_found` (a version number that was never issued).
- **Optional compare-and-set.** A set may carry `expected_version`. If it does
  and the current version differs, the write is refused with
  `version_conflict` and the current version number. A set without it always
  wins. Tools that do the read-then-set carry-forward pattern should pass it so
  a concurrent edit is not silently lost.
- **Anyone may set either text.** Any session with a bound session address may
  set a seat's charter or handoff, whether or not it holds the seat. Nothing
  prevents a holder rewriting its charter from `reviewer` to "dictator
  supreme"; the change is recorded with its author and is meant to be found in
  diagnosis, not prevented. A relieving session can write the handoff for a
  holder that has died.
- **Charter-change notice.** When a session other than the current holder sets
  the charter, the fabric sends the holder a notice (see *Service notices*).

## Operations

Every write takes the usual `producer_id`, `lease_token` and `operation_id`.
Exact retries replay the stored result, and reusing an operation ID for a
different request is `operation_conflict`, as for every other fabric write.
The acting session is named by `session_address`, which must be currently bound
to the calling adapter (the same fence as a message's sender today).

| Operation | Semantics |
| --- | --- |
| take | Succeeds only if the seat is vacant: it creates a holding and returns the seat, the new `holding_id`, the current charter and handoff (text and version), the count of queued seat mail, and a summary of the previous holding. Refused with `seat_held` and the current holder when the seat is held, even by the same session (the response says so, and the caller treats it as already held). Creates the seat if it does not exist. |
| leave | Ends the caller's holding. If no handoff version was set during this holding, the holding ends as `left_without_handoff` rather than being refused. Refused with `not_holder` if the caller does not hold the seat. |
| relieve | The only way into a held seat. Ends the current holding as `relieved`, starts the caller's holding in the same transaction, sends the previous holder a relief notice, and returns what take returns. Takes an optional note. Refused with `seat_vacant` if nobody holds the seat (take it instead). No confirmation from the current holder is needed, and a stale holder is relieved the same way. |
| set charter / set handoff | Write a new version of one text (see above). Creates the seat, vacant, if it does not exist. |
| retire | Ends any holding as `retired`, fails queued seat mail with `seat_retired`, and marks the seat retired. A later take or set recreates it with the next generation and empty texts; the old versions and holdings stay readable until retention prunes them. |
| list seats | Side-effect-free. Filters: `scope`, `holder` (a session address), `state` (`vacant`, `held`, `stale`). Returns each seat's address, state, holder (session address, adapter, started_at), charter version and the first line of the charter, handoff version, and queued seat-mail count. Never returns full texts. |
| get seat | Side-effect-free. One seat's record, current holding, and current charter and handoff in full. |
| get text version / list text versions | Side-effect-free. One version in full, or version metadata (number, author, set_at, length) for the versions still kept. |
| list holdings | Side-effect-free. The seat's holdings, newest first, within retention. |

All reads are open to anyone: a session receiving a message sent as a seat can
read that seat's charter and history without holding anything.

### Creating a seat

There is no separate create operation. `take`, `set charter` and `set handoff`
create a seat that does not exist. Sending a message to a seat that has never
been created is refused with `seat_not_found`, since a mistyped address should
fail loudly rather than queue forever.

## Seat mail

A message may be addressed to a seat. Today a recipient address resolves
through its binding, the submission records the recipient binding generation,
and a semantic rebind fails queued work. A seat changes holder far more often
than a session is rebound, and mail to a seat should reach whoever holds it
next, so seat mail follows its own rules:

- **Queue on the seat.** The delivery's recipient is the seat address, and the
  submission records the seat's `generation`. FIFO is per seat address and seat
  generation, with the same head-of-line rule as any other address.
- **Resolve the holder at claim time, not at submission.** An adapter claims
  seat mail with the seat address and the seat generation. The claim succeeds
  only for the adapter that owns the current holder's session binding, and only
  while that binding's generation still matches the holding; otherwise the claim
  reports `seat_vacant` or `holding_stale` and claims nothing. The claim result
  carries `holder_address`, the session to deliver into, and the delivery
  records `claim_holding_id`. The adapter delivers into the holder's session
  exactly as it would deliver mail addressed to that session, under the
  message's activation policy, using the holder binding's capabilities.
- **A change of holder carries mail over.** Taking, leaving or relieving never
  advances the seat generation, so queued seat mail waits through the change
  and goes to the next holder. The same transaction that changes the holder
  returns any pre-dispatch claim on the seat's mail to the queue, so the old
  claim token is fenced (`claim_fenced`) and the next holder claims the message
  first. A delivery already dispatching under the old holding is reconciled
  normally and stays delivered to the session that got it.
- **Vacant seats queue.** While a seat is vacant its mail stays queued. The
  sender's TTL still applies, and mail that expires while the seat is vacant
  expires as usual. A sender that wants mail to wait out a long vacancy sets a
  long TTL.
- **Retirement fails mail.** Retiring a seat advances its generation, and mail
  queued under the old generation fails with `seat_retired`, the same way a
  semantic rebind fails session mail.
- **No ordering across addresses.** A session that holds seats has one FIFO for
  its own address and one per seat. The fabric orders nothing across them.

`take` and `relieve` report the queued count; they never return, claim or
deliver mail. As everywhere in the fabric, only adapter claiming moves work.

## Sending as a seat

A submission sends as a seat by naming its own session as `sender_address`
and the seat as `as_seat`. It is accepted only if that session (at its current
binding generation and adapter) holds the seat; otherwise it is refused with
`not_holder`. That check is how the fabric enforces single holding: a relieved
session cannot keep speaking for the seat, whatever it believes.

The accepted envelope's `sender_address` is the seat and its
`sender_generation` is the seat generation, so replies and rounds match the
seat with the existing exact-address rules. Three more fields keep the real
sender: `sender_session` (the session that wrote it), `sender_holding_id`, and
`sender_charter_version` (the charter version current when it was sent, so a
recipient reading the charter later can see whether it has changed).
Provenance never loses who actually wrote the message.

A reply to a message sent as a seat goes to the seat, not to the session that
sent it, so a thread keeps going across handoffs. Rounds match a reply that
arrives from the seat they were addressed to.

Drivers render seat provenance in the delivered text, for example
`from s/5b69… (crew-claude) as lead@rusty-engine` and, for seat mail,
`to lead@rusty-engine (your seat)`.

## Stale holdings

A holding has no lease of its own, because the seat outlives the session's
attention: agents work turn by turn, cannot send heartbeats, and an idle
session is not a dead one. A holding is **stale** when its holder's session
binding is gone (`holder_unbound`), rebound to another native session or
adapter (`holder_rebound`), or owned by an adapter with no live lease
(`adapter_lease_expired`).

A stale holding is shown as stale in listings with its `stale_reason`. For an
expired adapter lease, `stale_since` is the lease's expiry; the fabric does not
know when a binding went away, so the other reasons carry no time.
It is never vacated automatically, its seat mail stays queued, and `take` is
still refused. Any session may relieve a stale holder the usual way. When the
holder's driver comes back and the binding still matches, the holding is no
longer stale, and queued seat mail flows again.

## Service notices

Relief notices and charter-change notices are messages from a service-owned
origin (`fabric/seats`), not from an agent address, as the spec requires of any
operator or system ingress. They are sent to the affected session address with
`next_turn`, and they say what changed, by whom (session, adapter, seats held),
and where to read the details. A relief notice tells the previous holder it no
longer holds the seat and that sending as it will be refused.

## Reason codes

| Code | Raised by |
| --- | --- |
| `seat_held` | take on a held seat; includes the current holder |
| `seat_vacant` | relieve on a vacant seat |
| `not_holder` | leave, or sending as a seat, by a session that does not hold it |
| `seat_not_found` | sending to, or reading, a seat that was never created |
| `seat_retired` | queued seat mail failed by retirement |
| `invalid_seat_address` | malformed `name@scope` |
| `invalid_request` | a binding write to a `name@scope` address, or an adapter ID starting with `fabric/` (reserved for service notices) |
| `version_conflict` | set with a stale `expected_version`; includes the current version |
| `version_pruned` | read of a version removed by retention |
| `version_not_found` | read of a version number that was never issued |

`left_without_handoff` is a holding `end_reason`, not an error. `seat_vacant`
and `holding_stale` are also claim reasons (nothing claimed), alongside the
existing `no_work` and `head_*` reasons.

## Retention

Text versions keep the newest `-seat-text-versions` per seat and kind
(default 50), pruned in the transaction that writes the next one. Seat
operation receipts are pruned with the same retention window as delivery
operation receipts. Seats, retired seats and ended holdings are small and are
kept for now; they get a retention bound if they ever need one.

## HTTP API

Writes take `adapter_id`, `lease_token`, `operation_id` and `session_address`
(plus an optional `note`) and return the seat, the resulting holding, the
previous or ended holding, and any notice message ID; `201` when new, `200` on an
exact replay.

| Route | Operation |
| --- | --- |
| `POST /v1/seats/{seat}/take` | take |
| `POST /v1/seats/{seat}/leave` | leave |
| `POST /v1/seats/{seat}/relieve` | relieve |
| `POST /v1/seats/{seat}/retire` | retire |
| `PUT /v1/seats/{seat}/charter`, `PUT /v1/seats/{seat}/handoff` | set text: `body`, optional `expected_version` |
| `GET /v1/seats?scope=&holder=&state=` | list seats |
| `GET /v1/seats/{seat}` | get seat, with full charter and handoff |
| `GET /v1/seats/{seat}/charter[?version=N]`, `.../handoff` | one text version (current by default) |
| `GET /v1/seats/{seat}/charter/versions`, `.../handoff/versions` | version metadata, newest first |
| `GET /v1/seats/{seat}/holdings` | holdings, newest first |
| `POST /v1/messages` with `as_seat` | send as a seat |

Errors return `{"code": …}` and, for `seat_held` and `version_conflict`, a
`detail` with the holding in the way or the current version.

## Driver and CLI surface

Each driver (crew-claude, crew-codex, crew-dsh) has the same subcommands
through the shared `crewcli` package, next to `send` and `who`. Hermes can call
the same HTTP operations. No harness needs more than its driver already does.

```
seat take SEAT [--note TEXT]
seat leave SEAT [--note TEXT]
seat relieve SEAT [--note TEXT]
seat retire SEAT [--note TEXT]
seat list [--scope SCOPE] [--state vacant|held|stale|retired] [--mine]
seat show SEAT
seat charter SEAT [--version N]
seat charter SEAT --set [--expect VERSION] [TEXT... | -]
seat handoff SEAT [--version N]
seat handoff SEAT --set [--expect VERSION] [TEXT... | -]
seat versions SEAT charter|handoff
seat holdings SEAT
send --as SEAT --to ADDRESS TEXT...
```

Reads go straight to the fabric (`CREW_FABRIC_URL`, by default
`http://127.0.0.1:8787`) and take `--json`. Writes go through the caller's own
driver at `POST /v1/seat-write`, because a seat write needs the adapter's lease
and the session's address. A fabric refusal comes back with its own status and
body, so the command can name the holding in the way, or the current version
to pass as `--expect`. `--set` with no text, or with `-`, reads the text from
stdin.

`who` has a SEATS column (the seats each session holds, marked stale where
they are). On each delivery pass, every driver lists the held seats once. For
each of its sessions it claims the session's own address first, then each seat
that session holds, at the seat's generation. A claim names the holder it
resolved. If that holder is another session (the seat changed hands since the
listing), the driver releases the claim, and the pass for that session takes
it. A stale holding is not in the listing, so its mail waits for relief.

Seat mail is rendered as "Crew message to your seat SEAT from …", and its reply
line is `send --as SEAT --to SENDER`. A message sent as a seat names the seat
and the session holding it. Notices from `fabric/seats` have no reply line. The
Claude Code and Codex session-start hooks name the seats a resumed session
still holds, framed as held and not as identity. They point at
`seat show SEAT` for the charter and handoff, and at writing a handoff before
leaving. DSH has no session-start hook. A DSH agent sees its seats with
`crew-dsh seat list --mine`.

The context-injecting surfaces are where a size limit may later prove
necessary. They do not inline charter text yet, only the pointer to
`seat show`. No limit is set.

## Decisions

Agreed with Patch on 2026-10-31.

- **Name:** "seat", not "role" (see *Why "seat" and not "role"*).
- **Scope:** an opaque namespace in `name@scope`; the fabric has no scoping
  rules and no global seats.
- **Several seats per session:** allowed, with no limit.
- **Handoff location:** in the fabric, beside the charter, not in Den. Seats
  for a roundtable often have no Den task, the core must not know Den concepts,
  and keeping both texts with the seat lets `take` return everything in one
  read. A Den pointer can be added later by an adapter if it proves useful.
- **One charter, one handoff, versioned:** set never appends; a bounded FIFO of
  past versions stays readable for curiosity and diagnosis.
- **No length limits** on stored text; enforce later on the context-injecting
  surfaces if it becomes a problem.
- **Open reads, free edits:** every seat's texts and history are readable by
  anyone, and any session may set either text, with full provenance. Surprising
  edits are left to be found in diagnosis.
- **Stale, not auto-vacated:** a dead holder's seat is marked stale and stays
  held until someone relieves it.
- **Leaving without a handoff** is allowed and recorded as
  `left_without_handoff`.

## Proving slice

Service tests prove:

These are built (`internal/sqlite/seat_test.go`, `internal/httpapi/seat_test.go`):

1. A second take of a held seat is refused with `seat_held` naming the holder.
2. Relieve ends the old holding, starts the new one atomically, and sends the
   old holder a relief notice.
3. A relieved session sending as the seat is refused with `not_holder`.
4. Mail sent to a vacant seat is delivered to the next holder in FIFO order,
   and a change of holder does not fail queued seat mail.
5. A claim made before a change of holder returns to the queue and its token
   can no longer begin dispatch.
6. Retiring a seat fails its queued mail with `seat_retired`.
7. A stale holding stays held, keeps its mail queued, and is relieved normally.
8. Setting a text creates a version with provenance; the oldest version past
   the bound returns `version_pruned`; a stale `expected_version` is refused.
9. A reply to a message sent as a seat is addressed to the seat.
10. Listing, show, version and holding reads never claim, wake or deliver.
11. Seats survive a fabric restart.

The end-to-end demo spans two harnesses (for example Claude Code and Codex):
one session takes `lead@<scope>`, the other is refused, the second relieves the
first after a handoff is set, a message sent to the seat while it changes hands
reaches the new holder, and `seat list` shows the right holder at each step.
