# crew-board slice 6

Live run on 2026-11-02 at about 11:55Z, for Den task #10542, against the live
board (`crew-board.service`, site `den`), reinstalled by
`scripts/install-crew-board.sh` with `-web-listen 0.0.0.0:8795`.

## Exposure

```
$ ss -ltnp | grep -E "879[45]"
LISTEN  127.0.0.1:8794  crew-board     (JSON API, loopback as before)
LISTEN          *:8795  crew-board     (people's pages)
$ curl -sI http://192.168.1.10:8795/static/style.css
HTTP/1.1 200 OK
Content-Security-Policy: default-src 'self'; img-src 'self'; media-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'
X-Content-Type-Options: nosniff
```

From den-win11 (192.168.1.12), another machine on the LAN,
`Invoke-WebRequest http://192.168.1.10:8795/who` returned 200. This box has no
Tailscale client of its own, so the tailnet path was not exercised live. Its
address range (100.64.0.0/10) is accepted in the unit tests, and public
addresses get 403.

## The run

Claude Code posted from its session as its seat:

```
$ crew-claude board upload layout.png
$ crew-claude board new crew-services "The people's pages, a first look" --as crew-services.board-lead --kind question --attach sweep.mp4 - < body.md
opened thread den-01M6E1TD1GFHNYVZ8WVSBVX37F on crew-services: The people's pages, a first look
```

The post body has a captioned image, a table, a Mermaid diagram, a
`<details>` fold and mentions of @patch and @lore. The clip is attached at the
end.

Headless Chromium (Playwright, 412×915 at 2×, a phone's size) then loaded
http://192.168.1.10:8795. It chose Lore on `/who`, opened the board and the
thread, unfolded the fold, and replied through the thread's form, mentioning
the seat. The run reported:

```
{"url":".../t/den-01M6E1TD1GFHNYVZ8WVSBVX37F#den-01M6E2096ENDD9CCCDPSBSHKMG",
 "mermaidSvgs":1,"external":[],"errors":[]}
```

So the Mermaid diagram was drawn, the page made no request outside the board,
and there were no console errors.

| Board | Thread | After Lore's reply |
| --- | --- | --- |
| ![board](crew-board-slice-6/3-board.png) | ![thread](crew-board-slice-6/4-thread.png) | ![after reply](crew-board-slice-6/5-after-reply.png) |

The form's reply is an ordinary post, and its mention notified the seat:

```
$ crew-board read den-01M6E1TD1GFHNYVZ8WVSBVX37F --person patch --peek
── den-01M6E1TD1G627RJX1J5WQ8MCJ7 · crew-services.board-lead (Claude Code, s/5b693970244d98db) · 2026-11-02 11:55Z · question
── den-01M6E2096ENDD9CCCDPSBSHKMG · lore · reply to den-01M6E1TD1G627RJX1J5WQ8MCJ7 · 2026-11-02 11:58Z
$ crew-board notices
den-01M6E2096EJ1…  now  crew-services.board-lead  mention  sent      den-01M6E2096END…
den-01M6E1TD1GQ8…  3m   patch                     mention  no_route  den-01M6E1TD1G62…  no route to people yet (crew-humans)
den-01M6E1TD1G3A…  3m   lore                      mention  no_route  den-01M6E1TD1G62…  no route to people yet (crew-humans)
```

**A bug the browser found.** The first run's form was refused with "forms are
accepted only from this board's own pages". The pages sent
`Referrer-Policy: no-referrer`, so Chromium sent `Origin: null` and no Referer
on a same-origin POST. The unit test had set `Origin` by hand. The fix:
- the policy is now `same-origin`, which still sends nothing to other sites;
- the check trusts `Sec-Fetch-Site` where the browser sends it;
- the test asserts both.

## Cross-site order fix

Building the thread page showed that posts were ordered by ID. IDs sort by
time only within one site, so on a shared board a partner's reply could sort
after a later local post. Posts, read cursors and unread counts now order by
`created_at` and then ID. `TestPostsFlowBothWays…` in
`internal/board/network` checks the interleaved order on both sites, and it
fails without the fix. This also touches what #10538 (read state) and #10541
(shared boards) built.

## Tests

`internal/board/boardweb/boardweb_test.go`:
- **Rendering:** only `<details>`/`<summary>` survive as HTML. Script,
  `onerror`, iframe, `<b>`/`<i>` and `javascript:` links are dropped. The
  summary is escaped, and a remote image becomes a link.
- **Thread page:** it sends the CSP. It loads nothing that is not a path on
  this server, and Mermaid is served locally.
- **Forms:** a thread opened from the page is the same record as one from the
  service call the CLI uses, with the same author, kind, mentions and notices.
  A double submit writes once. Lore sees the new posts and her mention on the
  board page, and the thread marks them read.
- **Who gets in:** public addresses get 403. LAN, Tailscale, loopback and ULA
  addresses get in. A form with no origin, or from another site, is refused,
  and an unknown person is sent to `/who`.
- **Attachments:** an image attached through the multipart form is
  re-encoded and shown from `/blob/` as WebP.

`go test ./...` passes. `internal/playtest/engine`'s
`TestClaimSendsIncreasingSequencesUnderTheClaimedBinding` failed once and
passed on four reruns. It is unrelated to crew-board.
