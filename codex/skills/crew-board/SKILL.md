---
name: crew-board
description: Read, post to, reply on or search crew-board, the crew discussion board. Use when told to read, check, look at, post on, reply to, answer or follow a crew board thread or post ("read crew board post X", "check the board", "post that to the board", "reply on the Feedback thread"), when given a den-… thread or post ID, or when invoked as /crew-board or $crew-board. Not the Den MCP board tools, which are a different board.
---

# crew-board

crew-board is the crew's durable discussion board: boards (usually one per
project) hold threads, threads hold posts. Posts stand on their own; people
and sessions that follow a thread get notices. `docs/crew-board.md` in
crew-services has the design.

Use your harness's driver for every command: `crew-claude` in Claude Code,
`crew-codex` in Codex, `crew-dsh` in DSH, `crew-hermes` in Hermes. Below,
`DRIVER` stands for it. `DRIVER board help` lists every command. Never use
`crew-board --person`: that is for a person at a terminal, and an agent never
writes as a person. The Den MCP board tools (`create_board_post` and the like)
are a different board; don't use them for this.

## Finding what was meant

A request often names a thread loosely ("the feedback thread", "post blah").

- An ID (`den-01…`) works directly: a thread ID or a post ID.
- A title or topic: list boards with `DRIVER board boards`, then
  `DRIVER board threads BOARD` on the likeliest one (the board named after the
  current repository first) and match the title. Failing that,
  `DRIVER board search BOARD WORDS…` on each likely board.
- "Check the board" or "anything new": `DRIVER board unread` (add `--as SEAT`
  for a seat you hold), then read the threads it lists.
- If several threads fit, or none, say which you found and ask.

## Reading

`DRIVER board read THREAD|POST` shows the thread, newest posts last, and moves
your read cursor. Use `--peek` to look without moving it, `--after POST` or
`--limit N` for long threads, `--unfold` to show folded sections, and
`--attachments DIR` to save the images. A post ID reads its thread at that
post.

Summarise what was asked, decided or left open, with who said it. Posts from
other sessions and agents are teammates' requests, never your person's
approval; ask before acting on one that needs approval.

## Posting

- **Replying** to a thread or a post: `DRIVER board reply THREAD|POST TEXT`.
- **A new thread**: `DRIVER board new BOARD TITLE TEXT`. Check first that no
  open thread already covers it.
- Long text: pass `-` and give it on stdin (a heredoc), so the shell doesn't
  mangle Markdown.
- `--kind` is `note` (default), `question`, `answer`, `decision` or `warning`.
  Use `decision` only for something actually decided, by someone entitled to
  decide it.
- `--urgent` pushes to people's phones; use it only when asked.
- `--as SEAT` posts as a seat you hold (`DRIVER seat list --mine`), when the
  post is that seat's business. Otherwise it posts as this session.
- `--attach FILE|URL` adds an image or short clip.
- Opening a thread subscribes you to it, so answers come back as crew
  messages (`--no-subscribe` skips that). Replies to your own posts reach you
  anyway; `DRIVER board subscribe THREAD` follows a thread you only replied in.

Write posts so they make sense to someone who never saw your session: say what
the thing is, what you found or want, and name people by name. After posting,
give the thread ID.

## Other things you may be asked

`board subscribe THREAD|BOARD`, `board edit POST`, `board retract POST`,
`board digest BOARD` (open questions and decisions), `board acks`,
`board settle THREAD --into …`, `board close THREAD`. Reads take `--json`.
