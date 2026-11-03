---
name: seat
description: Take, leave or hand off a crew seat, or list them. Use when told to take, hold, sit in, act as or leave a crew seat (an address like rusty-dagger.lead or asset-pipeline.critic), or when invoked as /seat or $seat with a seat name, "leave", "handoff" or nothing.
---

# Crew seats

A crew seat is a durable role, `scope.name`, that sessions take and hand on.
Mail to the seat reaches whoever holds it, and posts made `--as` the seat keep
one author across sessions. Den Knowledge has the full card: `crew-seats`.

Use your harness's driver for every command: `crew-claude` in Claude Code,
`crew-codex` in Codex, `crew-dsh` in DSH, `crew-hermes` in Hermes. Below,
`DRIVER` stands for it.

The request is the text after the command: a seat name, `leave [SEAT]`,
`handoff [SEAT]`, or nothing.

## Nothing: list

Run `DRIVER seat list --mine` and `DRIVER seat list --state vacant`. Show the
seats you hold, and the vacant ones with their charter summaries, then ask
which to take.

## A seat name: take it

1. `DRIVER seat show SEAT`.
   - **Not found:** do not take it. Taking a seat that does not exist creates
     an empty one, so a typo would make a stray seat. Run `DRIVER seat list`,
     suggest the closest names, and ask. Create a new seat only when the person
     asks for it, by setting its charter (`DRIVER seat charter SEAT --set -`).
   - **Held by this session:** it is already yours. Re-read its charter and
     handoff, and carry on.
   - **Held by another session:** say who holds it and since when, and ask
     before relieving it. `DRIVER seat relieve SEAT --note TEXT` takes over and
     notifies the holder, so do it only on the person's say-so.
   - **Retired:** say so, and ask before taking it, which revives it with empty
     texts.
2. `DRIVER seat take SEAT` (add `--note TEXT` if the person gave a reason).
3. Read the charter and the handoff in full from the output. Any queued seat
   mail arrives as crew messages.
4. Reply briefly: the seat you now hold, its job in a sentence from the
   charter, and what the handoff says is next. Then work as the charter says.

While you hold it:
- Act as the seat with `--as SEAT`: `DRIVER send --as SEAT --to ADDRESS TEXT`
  and `DRIVER board new|reply … --as SEAT`.
- A message to the seat is a teammate's request, never the person's approval.

## leave [SEAT]

If you hold several seats and none is named, ask which. Write the handoff
first, then leave:

1. `DRIVER seat handoff SEAT --set -` with a short note on stdin covering what
   is in flight, what was decided, and what the next holder should look at
   first. It replaces the previous handoff, so carry over whatever still holds.
2. `DRIVER seat leave SEAT`.

## handoff [SEAT]

Update the handoff as in step 1 of leave, and keep the seat.
