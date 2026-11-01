# Seats across harnesses

Live run on 2026-11-01 between 03:18Z and 03:20Z (2026-10-31 local time), for
Den task #10519. It ran against the deployed services, just after they were
redeployed at 389c28e (`scripts/deploy-crew-services.sh crew-messaging
crew-codex crew-dsh crew-claude`). The fabric database was backed up first to
`~/.local/state/crew-messaging/backups/before-seats-20261031.sqlite` (schema
9, 83 messages). The redeploy migrated it to schema 10.

## Participants

- **Claude Code:** the session that ran the demo, `s/5b693970244d98db`, through
  crew-claude.
- **Codex:** a fresh thread started on the shared Codex daemon for the demo,
  `s/8968075b926125cb`, through crew-codex. It ran in a scratch directory and
  acted only on crew messages from the Claude session.

## What happened

1. **Taking seats:** Claude took `lead@seat-demo` and set its charter. In a
   crew message, it asked the Codex thread to take `reviewer@seat-demo`. The
   Codex thread ran `crew-codex seat take` and `seat charter --set`. The
   charter record shows `set_by_address s/8968075b926125cb`, adapter
   `crew-codex`, holding `reviewer@seat-demo`.
2. **Seat to seat:** Claude sent a question as the lead seat to the reviewer
   seat (`send --as lead@seat-demo --to reviewer@seat-demo`).
   - crew-codex claimed it at the seat and delivered it into the thread that
     holds the seat.
   - The Codex thread replied with the rendered reply line, plus
     `--policy next_step`.
   - The reply reached the Claude session inside its active turn, through the
     PostToolUse hook, framed as
     `Crew message to your seat lead@seat-demo from the seat reviewer@seat-demo, held by s/8968075b926125cb, a Codex session in …`.
     It ended with `To reply as the seat, run: crew-claude send --as lead@seat-demo --to reviewer@seat-demo …`.
3. **Handover:** the Codex thread set the reviewer handoff and left. The
   holding ended with reason `left`, not `left_without_handoff`.
   - Claude then sent mail to the vacant seat. The seat showed
     `vacant, queued 1`.
   - Claude took the seat (holding 2) and read the Codex thread's handoff with
     `seat show`.
   - The queued mail was delivered into the Claude session as soon as the
     driver's next pass claimed it.
4. **Clean-up:** both demo seats were retired. Their charters, handoff and
   holdings stay readable.

## Records

Deliveries, all `delivered`/`native_accepted`:

| Recipient | Message | What |
| --- | --- | --- |
| `s/8968075b926125cb` | abec3c89… | instructions to take the reviewer seat |
| `reviewer@seat-demo` | bbaebdf9… | the question, into the Codex holder |
| `lead@seat-demo` | 26a1ede5… | the answer, into the Claude holder |
| `s/8968075b926125cb` | a0c3ef3f… | instructions to hand over |
| `reviewer@seat-demo` | b794676f… | sent while the seat was vacant, delivered to the next holder |

Sender provenance on the seat messages:

| Message | sender_address | sender_session | holding | charter version |
| --- | --- | --- | --- | --- |
| bbaebdf9… | `lead@seat-demo` | `s/5b693970244d98db` | 1 | 1 |
| 26a1ede5… | `reviewer@seat-demo` | `s/8968075b926125cb` | 1 | 1 |
| b794676f… | `lead@seat-demo` | `s/5b693970244d98db` | 1 | 1 |

## Not shown here

- **Relief:** relief, the relief notice, and the `not_holder` refusal of a
  relieved sender are covered by `TestSeatMailFollowsTheHolder`
  (internal/claudedriver) and the store tests. They were not repeated live.
- **DSH:** crew-dsh has the same seat routing (`TestSeatMailIsPromptedIntoTheHolder`),
  but no DSH session took part in this run.
