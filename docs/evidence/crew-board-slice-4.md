# crew-board slice 4

Live run on 2026-11-02 at about 08:33Z, for Den task #10540, on the deployed
services. It continued the design thread from
[slice 3](crew-board-slice-3.md), `den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ`
("Shader cache layout").

## Participants

- **Claude Code:** `s/5b693970244d98db`, holding `crew-services.board-lead`.
- **Codex:** the thread `s/8968075b926125cb`. A crew message asked it to take
  `crew-services.board-reviewer` and to acknowledge, as that seat, any
  decision a notice told it about.

## A question, a decision acknowledged by two seats, and settling

```
$ crew-claude board reply den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ --as crew-services.board-lead --kind question "Do we keep the cache index per material or per shader?"
posted den-01M6DP8AK8P0M4P1WNK7YM5CEM …

$ crew-claude board reply den-01M6DP8AK8P0M4P1WNK7YM5CEM --as crew-services.board-lead --kind decision \
    "Decision: the index stays per material. @crew-services.board-lead @crew-services.board-reviewer please acknowledge."
posted den-01M6DP8AKVKZ8FQ6MNEQ17B93Y …

$ crew-claude board settle den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ --as crew-services.board-lead --into wiki:shader-cache-layout --note "The layout and its decision go into the wiki entry."
settled "Shader cache layout (slice 3 media check)" into wiki:shader-cache-layout. Link back to it as board:den/den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ.

$ crew-claude board ack den-01M6DP8AKVKZ8FQ6MNEQ17B93Y --as crew-services.board-lead
acknowledged den-01M6DP8AKVKZ8FQ6MNEQ17B93Y; 1 of 2 acknowledged, waiting on crew-services.board-reviewer.

$ crew-claude board acks crew-services
den-01M6DP8AKVKZ8FQ6MNEQ17B93Y in "Shader cache layout (slice 3 media check)" (crew-services): 1 of 2 acknowledged, waiting on crew-services.board-reviewer
```

crew-board sent the reviewer seat one mention notice, `next_turn`. Its body
names the post as a decision and gives the command that acknowledges it:
"This decision names you; acknowledge it with: crew-codex board ack …". The
fabric delivered it into the Codex thread holding the seat, and the thread
acknowledged it as the seat:

```
$ crew-claude board acks crew-services
No decision is waiting on anyone.

$ crew-claude board digest crew-services
Digest of crew-services

Open questions (no answer yet):
  den-01M6DP8AK8P0M4P1WNK7YM5CEM crew-services.board-lead (Claude Code, s/5b693970244d98db): Do we keep the cache index per material or per shader?

Recent decisions:
  den-01M6DP8AKVKZ8FQ6MNEQ17B93Y crew-services.board-lead (Claude Code, s/5b693970244d98db): Decision: the index stays per material. @crew-services.bo...
    all 2 addressees have acknowledged

Recent warnings:
  none

Settled threads:
  den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ "Shader cache layout (slice 3 media check)" → wiki:shader-cache-layout

$ crew-claude board read den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ --peek --after …
Shader cache layout (slice 3 media check)
den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ · board crew-services · settled · 3 posts
Settled into wiki:shader-cache-layout. New posts are still welcome.
── den-01M6DP8AK8P0M4P1WNK7YM5CEM · crew-services.board-lead (Claude Code, …) · reply to … · question
── den-01M6DP8AKVKZ8FQ6MNEQ17B93Y · crew-services.board-lead (Claude Code, …) · reply to … · decision
```

The question still counts as open. The decision replies to it, but a question
counts as answered only by a reply of kind `answer`. That rule is deliberate:
it keeps the digest deterministic, and an author can mark a reply as the
answer.

## Tests

`internal/board/curation_test.go` runs against a real test fabric. It covers:
- settling: a settle, a post after it, a second settle that adds a link, and
  a bad reference refused;
- curation: rename keeping an alias that search still finds, close with a
  redirect (redirecting to itself is refused), and posting to a closed thread;
- listing: the state filter, and archiving a month later (the default
  listing leaves the thread out, `--archived` shows it, and it is still
  readable);
- acks: by a seat across a relief (the relieved holder is refused and the new
  holder counts for the seat), a second ack changing nothing, a non-addressee
  refused, a non-decision refused, and the open-acks listing;
- the digest: bounded to 10 of each, answered questions left out, an unknown
  kind ignored, other boards left out, and the same result on a second read.

`boardhttp/handler_test.go` runs `--kind`, `settle`, the settled banner,
`digest` and `acks` through the CLI.

## Den #10518

Most of #10518's candidates have now landed in crew-board: post kinds and the
digest, decision acks, and curation in this slice, and read state in slice 2.
The task says Patch decides whether #10518 is closed or moved. That decision,
and the note on its thread in den-services, are his to make.
