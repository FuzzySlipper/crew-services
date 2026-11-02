# Seats across harnesses

These runs predate the 2026-11-02 change of seat addresses from `name@scope`
to `scope.name`. Today `lead@seat-demo` would be `seat-demo.lead`, and
`lead@crew-services` would be `crew-services.lead`. The output below is left as
it was recorded.

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

## Refusal and relief

A second live run on 2026-11-02 between 02:24Z and 02:30Z, with the same two
sessions, on `lead@crew-services`. Commands and output are excerpts: the
listings drop the charter-summary column, and lines that repeat earlier output
are cut (marked `…` where cut mid-text). The full log of the run is in the
task thread.

1. **Claude takes the seat** and sets its charter:

   ```
   $ crew-claude seat take lead@crew-services --note "relief demo for #10519"
   you hold lead@crew-services (holding 1).
   $ crew-claude seat list --scope crew-services
   SEAT                STATE  HOLDER                    CHARTER  HANDOFF  QUEUED
   lead@crew-services  held   s/5b693970244d98db (you)  v1       v0       0
   ```

2. **Codex is refused**, and the refusal names the holder (the Codex thread's
   own report):

   ```
   $ crew-codex seat take lead@crew-services --note "codex wants the lead seat"
   Exit code: 1
   seat take lead@crew-services: seat_held: held by s/5b693970244d98db (claude-code) since 2026-11-02T02:24:12.948548564Z; `seat relieve` takes it over and tells them
   $ crew-codex seat list --scope crew-services
   lead@crew-services  held   s/5b693970244d98db  v1       v0       0
   ```

3. **Claude writes the transition note**:

   ```
   $ crew-claude seat handoff lead@crew-services --set "Transition note from s/5b693970244d98db: the relief demo is half done. Next holder: confirm you read this, then answer lead@crew-services mail."
   set the handoff of lead@crew-services to version 1 (143 characters).
   ```

4. **Codex relieves Claude**, reads the handoff, and sends as the seat:

   ```
   $ crew-codex seat relieve lead@crew-services --note "codex relieving claude for the demo"
   you hold lead@crew-services (holding 2), relieving s/5b693970244d98db. Read the handoff with `seat show lead@crew-services`.
   $ crew-codex seat show lead@crew-services
   held by s/8968075b926125cb (crew-codex) since 2026-11-02T02:26:40.342939511Z, holding 2
   Handoff (version 1, set 2026-11-02T02:26:33.794330364Z by s/5b693970244d98db (claude-code) holding lead@crew-services):
   Transition note from s/5b693970244d98db: the relief demo is half done. …
   $ crew-codex send --as lead@crew-services --to s/5b693970244d98db "lead@crew-services here: I read the handoff and hold the seat now."
   sent 98bc2d0a… from lead@crew-services to s/5b693970244d98db
   $ crew-codex seat list --scope crew-services
   lead@crew-services  held   s/8968075b926125cb (you)  v1       v1       0
   ```

   In the same transaction the fabric queued a relief notice for Claude
   (message e2de84e4…, from `fabric/seats`, `next_turn`): "You no longer hold
   the seat lead@crew-services: s/8968075b926125cb (crew-codex) relieved you.
   Messages you send as lead@crew-services will be refused. … Their note:
   codex relieving claude for the demo". The seat message 98bc2d0a… carries
   `sender_address lead@crew-services` and `sender_session s/8968075b926125cb`.
   Both stayed queued while the Claude turn that ran the demo was active, as
   `next_turn` requires, and the Codex report sent with `next_step` waited
   behind them in the address's FIFO.

5. **The relieved session is refused**, and the holdings record the takeover:

   ```
   $ crew-claude send --as lead@crew-services --to s/8968075b926125cb "should be refused"
   send: not_holder: this session does not hold that seat now; `seat show` names its holder
   $ crew-claude seat holdings lead@crew-services
   HOLDING  HOLDER              ADAPTER      STARTED                         ENDED                           REASON    NOTE
   2        s/8968075b926125cb  crew-codex   2026-11-02T02:26:40.342939511Z  -                                         codex relieving claude for the demo
   1        s/5b693970244d98db  claude-code  2026-11-02T02:24:12.948548564Z  2026-11-02T02:26:40.342939511Z  relieved  relief demo for #10519
   ```

   The first try printed the raw `fabric HTTP 409: {"code":"not_holder"}`:
   the drivers' `/v1/send` wrapped fabric refusals in a 502. They now pass
   them through as `/v1/seat-write` does, and the run above is after that fix
   was deployed.

6. **Clean-up:** Claude retired the seat (`retired lead@crew-services; any mail
   still queued for it failed with seat_retired.`).

## Not shown here

- **DSH:** crew-dsh has the same seat routing (`TestSeatMailIsPromptedIntoTheHolder`),
  but no DSH session took part in either run.
