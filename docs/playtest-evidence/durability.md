# Playtest evidence durability and recovery

Playtest artifacts use separate publications. A successful input, target release,
local capture cleanup, screenshot publication, script journal entry, and script
state update are separate facts. A receipt never turns a failed evidence write
into a completed evidence claim.

## Artifact layout

The session service state directory contains one atomic `session-*.json` record
per session and, for every submitted program, `scripts/<script-id>/` containing:

- `source.js`, the original submitted program;
- `events.jsonl`, the append-only program history;
- `script.json`, the latest parseable program state; and
- `worker.log`, when the worker reached stderr setup.

Wolf capture keeps a lease-specific private artifact directory. It contains
immutable PNG observations, its own `events.jsonl`, and local Xvfb/Moonlight
logs. PNG publication syncs its temporary content, renames it, and syncs the
artifact directory before the journal entry that names it. The
private Moonlight logs can contain credentials and are not report attachments.

## Write and failure behavior

JSON state publication writes and syncs a uniquely named sibling, replaces the
old name, then syncs the parent directory. If encoding, writing, or the first
sync fails, the old complete JSON document remains untouched. Temporary files
are removed when their publication did not succeed.

JSONL records are appended and synced individually. A failed or short final
write can leave an incomplete tail with no trailing newline. Earlier complete
lines remain original evidence. The service does not truncate that tail, append
a made-up terminal record, rebuild a replacement journal, or replay the
program. When the terminal record cannot be persisted, the final `script.json`
is written as `failed` with a `persist final journal record` error when its own
state publication succeeds.

To recover a journal, retain the original file, read only newline-terminated
lines through the first malformed or incomplete tail, and retain that tail as
the interrupted-write witness. `source.js` and the previous parseable
`script.json` remain useful provenance. If `script.json` itself could not be
published, it remains the earlier parseable state and must not be interpreted
as a terminal receipt.

## Cleanup is independent from evidence completeness

Cancellation and timeout still run target input neutralization even when a
terminal journal write fails. A failed neutralization is reported as
`cleanup_uncertain`; it is not repaired by replaying input.

On Wolf release, `released: true` means the target cleanup reported success and
`local_capture_stopped: true` means this adapter stopped its owned local capture
processes. If writing the local release journal fails, the same receipt carries
`evidence_error` and the operation returns an error. The session service keeps
the session `stopped` after successful cleanup but retains an `evidence
incomplete` diagnostic. If its own final session-state publication fails, it
also adds `evidence_error` to the returned release receipt. It does not describe
either case as unresolved cleanup.

After a service restart, active sessions and scripts are interrupted and are
never replayed. Stop or recover the session explicitly. Recovery starts a new
session; it does not repair, merge, or replace the earlier evidence.

## Retention limits

There is no automatic journal compaction, screenshot deletion, or evidence
reconstruction in this service. The script worker limits one program to 512 API
calls and 120 seconds, which bounds a single journal, but multiple program and
capture directories persist until the state directory is managed outside the
running service. Durability protects already published local artifacts against
failed replacements; it does not provide remote backup, infinite disk space, or
proof that a screenshot is a fresh game frame.
