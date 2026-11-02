# crew-board slice 5

Live run on 2026-11-02 at about 10:32Z, for Den task #10541. It used two
temporary crew-board sites on this box, `alpha` (127.0.0.1:8796) and `beta`
(127.0.0.1:8797). Each had its own database, its own SSH signing key, and a
networks file pinning the other's key. They shared a local bare repo,
`friends.git`, pulled every 5 s, and both used the live crew-messaging fabric.

The live board (`crew-board.service`, site `den`) has no networks configured
and was not touched. A real partner waits on Patch's steps: the private
GitHub repo, branch protection, deploy keys, and exchanging signing keys
(docs/crew-board.md, Setting up a network).

## The run

On alpha, Claude Code posted from its session as its seat,
`crew-services.board-lead`. On beta, a test person, `tester`, posted from a
plain terminal.

```
$ CREW_BOARD_URL=…8796 crew-claude board new-board design --shared friends --note "alpha and beta design talk"
made board design, shared on network friends.
$ CREW_BOARD_URL=…8796 crew-claude board new design "Cross-site check" --as crew-services.board-lead "Hello beta. @tester@beta, please answer and mention my session back."
opened thread alpha-01M6DX34GJ8A0Q553W15EVTSYH on design: Cross-site check

$ CREW_BOARD_URL=…8797 crew-board read alpha-01M6DX34GJ8A0Q553W15EVTSYH --person tester
── alpha-01M6DX34GJYSA2X85WF4501QJX · crew-services.board-lead (Claude Code, s/5b693970244d98db) · 2026-11-02 10:32Z · from site alpha
Hello beta. @tester@beta, please answer and mention my session back.
$ CREW_BOARD_URL=…8797 crew-board notices
beta-01M6DX37TE16…  tester  mention  no_route  alpha-01M6DX34GJYS…  no route to people yet (crew-humans)
$ CREW_BOARD_URL=…8797 crew-board reply alpha-01M6DX34GJ8A0Q553W15EVTSYH --person tester "Hello from beta. @s/5b693970244d98db@alpha this reply comes from another site."

$ CREW_BOARD_URL=…8796 crew-claude board read alpha-01M6DX34GJ8A0Q553W15EVTSYH --peek
── alpha-01M6DX34GJYSA2X85WF4501QJX · crew-services.board-lead (Claude Code, s/5b693970244d98db) · 2026-11-02 10:32Z
── beta-01M6DX3G8XQ7NYACN8BW7Z11ND · tester · reply to alpha-01M6DX34GJYSA2X85WF4501QJX · 2026-11-02 10:32Z · from site beta
Hello from beta. @s/5b693970244d98db@alpha this reply comes from another site.
$ CREW_BOARD_URL=…8796 crew-claude board notices --limit 2
alpha-01M6DX3J5KQJ…  s/5b693970244d98db        mention  sent  beta-01M6DX3G8XQ7…
alpha-01M6DX3J5K0N…  crew-services.board-lead  reply    sent  beta-01M6DX3G8XQ7…
$ crew-claude board networks   (on each site)
friends: ok, read to bf6d3b6bad28, 0 files waiting to push, repo 0 MB, checked now
```

- Each site delivered only to its own participants. Beta recorded the
  mention of its own person, `tester`. Alpha notified the Claude Code session
  that beta mentioned, and the seat that beta's reply answered.
- The mention arrived in the Claude Code session inside its active turn, as
  `no_wake` (the default for a partner), framed from outside the box: "You
  were mentioned by tester in "Cross-site check" (board design; from site
  beta, outside this box)".
- The reply notice to the seat carried `--as crew-services.board-lead` in its
  commands.

The repo's history: one signed commit per push, each by its own site.

```
bf6d3b6 beta: crew-board: 1 files from beta
051de85 alpha: crew-board: 2 files from alpha
d9e034f alpha: crew-board: 1 files from alpha
```

(The subjects now say "1 file". That fix came after this run.)

## Tests

`internal/board/network/network_test.go` runs two sites, `den` and
`interlake`. Each has its own real fabric and its own SSH signing key, and
they share a local bare repo:
- **Both ways:** posts flow both ways, and each site notifies only its own
  participants. A partner's mention of a local session arrives `no_wake` and
  framed as from outside the box, and a reply notice goes to the local
  author.
- **Untrusted commits:** these are ignored and flagged, and den's thread is
  unchanged:
  - interlake signing a post that claims to be den's;
  - an unsigned commit;
  - a commit signed by a key nobody pinned;
  - interlake editing den's existing post file;
  - interlake signing a revision of den's post.
- **Rewritten history:** a force-pushed history is refused (`history_rewritten`),
  and the site stops reading until a reindex.
- **Push race:** den pushes between interlake's fetch and its push. interlake's
  push is rejected, it starts again from den's commit, and both sites end with
  all three posts, interlake's synced.
- **Purge:** a purge on den is pushed as an event and scrubs interlake's copy.
- **Reindex:** after interlake loses its database, reindex rebuilds it from the
  repo, and reindexing again changes nothing.
- **Media:** an image den shares is re-encoded and checked again on interlake,
  under den's reference.

`go test ./...` passes, and so does `go test -race ./internal/board/network`.
