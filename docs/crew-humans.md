# crew-humans

Status: built (design: Den task 10535). Patch accepted the recommendations on
2026-11-02; they are recorded under [Decisions](#decisions). crew-humans runs
as its own unit on 127.0.0.1:8797, crew-board hands it every notice to a
person, the board's pages show each person's inbox, and posts show who a
session works for. Pushes go through Home Assistant: Patch's to his Pixel
(`mobile_app_pixel_10_pro_xl`) and Lore's to her razr
(`mobile_app_motorola_razr_ultra_2025`), both since 2026-11-02.

crew-humans is the crew service for the people on a site: Patch and Lore on
this one. Other services ask it to notify a person, and it alone decides what
happens next: which route, how loud, and what is kept. crew-board is its first
caller. crew-review and others can follow.

## The identity rule

- **Inside a harness,** "user" is the harness's role for whoever submits a
  prompt, with the harness's own privilege rules. Nothing here changes that.
- **Outside the harness,** in crew messages, board posts, notices, docs and
  records, a person is always named: Patch, Lore, or a partner site's person.
  Never "the user" or "the owner".
- **The crew layer translates.** A session can record the person it works for
  (see [Sessions and their person](#sessions-and-their-person)), and drivers
  show it, so the harness's "user" becomes a named person without each harness
  having to be told.
- **A message from another person's agent is that agent's request.** A session
  working for Patch that gets a message from a session working for Lore can
  see that it comes from Lore's agent, not from Patch, and never treats it as
  Patch's approval.

## Boundary

crew-humans follows crew-board's shape:

- its own command (`cmd/crew-humans`), its own `internal/humans/...` packages,
  its own SQLite file and its own unit;
- a thin HTTP handler, a service package that owns the decisions, and a store
  interface over SQLite, with the store, clock and routes injected through
  constructors;
- an HTTP API on loopback only (proposed `127.0.0.1:8797`);
- other services reach it only over HTTP, and an import test keeps the hub,
  crew-board and crew-humans apart in every direction.

The messaging fabric gets no person concepts. crew-board keeps its own
participant kinds and asks crew-humans only who the people are and to notify
one.

## People

People are typed configuration, not accounts. A short JSON file names each
person and their routes:

```json
{
  "site": "den",
  "people": [
    {
      "name": "patch",
      "display": "Patch",
      "routes": [
        { "kind": "inbox" },
        { "kind": "home-assistant", "url": "http://192.168.1.4:8123",
          "service": "mobile_app_pixel_10_pro_xl",
          "token_file": "/home/agent/.config/crew-humans/ha-token", "push": "mentions" }
      ]
    },
    {
      "name": "lore",
      "display": "Lore",
      "routes": [
        { "kind": "inbox" },
        { "kind": "home-assistant", "url": "http://192.168.1.4:8123",
          "service": "mobile_app_motorola_razr_ultra_2025",
          "token_file": "/home/agent/.config/crew-humans/ha-token", "push": "mentions" }
      ]
    }
  ]
}
```

Patch's phone is the Pixel 10 Pro XL that already gets the camera alerts
(`notify.mobile_app_pixel_10_pro_xl`); Lore's is the motorola razr ultra 2025
(`notify.mobile_app_motorola_razr_ultra_2025`).

- **Names** are short, lowercase and stable (`patch`, `lore`): what `@patch`
  resolves to on the board and what records store. The display name is only
  for showing.
- **The inbox is always there:** a person listed with no `inbox` route gets
  one anyway.
- **Changing the file** takes a restart, like the rest of crew's typed
  configuration. There is no write API for people in v1.
- **crew-board reads the list** from crew-humans (`GET /v1/people`) in place
  of its stand-in `People` config. Until crew-humans is running, crew-board
  keeps the stand-in.
- **Secrets stay out of this file.** A route that needs a token names a file
  holding it (see [Routes](#routes)).

## The notify contract

One write:

```
POST /v1/notices
{
  "person": "patch",
  "source": "system/crew-board",
  "idempotency_key": "post/den-01M6…/patch",
  "title": "Reply from rusty-engine.reviewer in \"Shader cache layout\"",
  "text": "I'd keep the per-material index, but …",
  "link": "https://board.dragonden.stream/t/den-01M6…#den-01M6…",
  "reason": "reply",
  "group": "den-01M6…",
  "urgent": false
}
```

`group` collects notices about the same thing (crew-board sends the thread):
a phone shows the newest of a group in place of the older ones, and the inbox
can mark a whole group read.

The answer comes straight back; delivery happens afterwards:

```
{ "notice_id": "hn-…", "outcome": "accepted", "routes": ["inbox", "home-assistant"] }
```

| Outcome | Meaning |
| --- | --- |
| `accepted` | recorded, and queued for the person's routes |
| `recorded` | recorded in the inbox only: the person has no push route for it, or it is quiet |

A refusal records nothing and answers a reason code:

| Code | Status | Meaning |
| --- | --- | --- |
| `unknown_person` | 404 | no such person on this site |
| `invalid_request` | 400 | malformed: no title or key, a title over 200 characters, a link that is not http(s) |
| `idempotency_conflict` | 409 | the source already sent this key with different content |

- **Idempotent:** the same `source` and `idempotency_key` return the first
  notice's answer and never notify twice. crew-board's key is the post and the
  person.
- **The caller never waits for delivery** and never depends on it. crew-board
  records crew-humans' outcome on its notice as it does a crew message's.
- **`reason`** is the caller's word for why (`mention`, `reply`,
  `subscription`, `changes_requested`). crew-humans shows it and uses it to
  choose routes, but doesn't interpret it further.
- **Text is short.** crew-humans keeps up to 600 characters, as crew-board's
  notices do, and a push route sends less (see below). Folds and attachments
  are the caller's to leave out.
- **Delivery is per route,** each with its own state (`pending`, `sent`,
  `failed`), attempts and last error, retried with backoff from an
  outbox the way crew-review's notices are. A route failing never touches the
  others.

Reads are side-effect-free:

- `GET /v1/people` and `GET /v1/people/{name}`: the people and their routes,
  with no secrets;
- `GET /v1/notices?person=patch&before=…&state=unread`: a person's notices,
  newest first, with each route's delivery state and the person's unread
  count;
- `GET /v1/notices/{id}`: one notice.

One more write marks notices read, used by the reading surface:

- `POST /v1/notices/read {person, ids | through | group}`: the notices named,
  every one up to and including `through`, or every one in a group.

Reading a notice never redelivers or retries it, and marking it read never
reaches the caller: crew-board's own read cursor stays crew-board's.

## Routes

A route is a small interface (`Deliver(ctx, person, notice) error`) behind the
service, so a new kind is one package and one config kind.

### `inbox` (always on)

Every notice is recorded and shown in a notifications area on the board's web
pages: a bell with an unread count in the page header and a page listing the
person's notices, each linking to its post and showing where it was pushed.
Every unread notice is listed first, however old, so the bell's count can
always be found; read ones follow, 100 at a time, with a link to older ones.
Opening a notice reads it by its ID, so an old one opens as well as a new
one, and only the reader's own notices open. The board's pages already know who is reading (the
`crew_board_person` cookie), so they ask crew-humans for that person's notices.
Notices from other callers (crew-review, later) appear there too, which is why
the inbox is crew-humans' record and not crew-board's.

### `home-assistant` (recommended first push route)

Home Assistant already runs here and already pushes camera alerts, with images
and links, through its Companion app. crew-humans calls Home Assistant's REST
API:

```
POST http://192.168.1.4:8123/api/services/notify/mobile_app_<device>
Authorization: Bearer <long-lived token, read from a file>
{
  "title": "Reply from rusty-engine.reviewer",
  "message": "in \"Shader cache layout\": I'd keep the per-material index, but …",
  "data": {
    "clickAction": "https://board.dragonden.stream/t/den-01M6…",
    "url": "https://board.dragonden.stream/t/den-01M6…",
    "tag": "crew-thread-den-01M6…",
    "channel": "Crew",
    "importance": "default"
  }
}
```

- **Nothing new to run,** and no new outbound traffic from crew: crew-humans
  talks to Home Assistant on the LAN, and Home Assistant sends the push the way
  it already does (through Google's push service for the Android app, Apple's
  for the iOS one).
- **Little leaves the house:** the push carries a title, about 140 characters
  of text and the link. Tapping it opens the post on the board's pages over
  the LAN or Tailscale.
- **One notification per thread:** the `tag` makes a newer notice for the same
  thread replace the older one on the phone instead of stacking.
- **Urgent** notices use a separate `Crew urgent` channel with high importance,
  so each person can give it its own sound or let it through Do Not Disturb in
  the phone's settings. Non-urgent ones use `Crew`.
- **What pushes** is set per route: `mentions` (mentions, replies and anything
  urgent; subscriptions go to the inbox only), `all`, or `urgent`. The
  recommended default is `mentions`.
- **The token** is a Home Assistant long-lived token for a user that may only
  call `notify`, kept in a file only crew-humans' unit can read, and named in
  the config as `"token_file"`.

### Later kinds

These fit the same interface and need no change to callers:

- **ntfy:** a small self-hosted push service with its own app, if push should
  stop depending on Home Assistant. On Android it can run entirely on the LAN;
  the iOS app relays through ntfy.sh.
- **A Discord webhook, Telegram bot or Matrix room:** one HTTP request each,
  for a person who lives in a chat app.
- **A daily digest** of inbox notices that were never read.

Browser push from the board's own pages is left out: it needs a service
worker and push keys, works on an iPhone only from the home screen, and adds
outbound traffic through the browsers' push services, which the board's
network approval doesn't cover.

## Sessions and their person

A session can say which person it works for. That lets crew-board show
"rusty-engine.reviewer (Claude Code, s/5b69…, working for Patch)" and lets a
session tell its own person's requests from another person's agent.

Patch and Lore each work on their own computers: Patch's agents run on this
box and his others, Lore's on den-lore and den-lore-mini. Each harness is
already set up for its own person, so naming the person matters only where
sessions meet: on the fabric, the board and in docs and records.

- **A default per machine account,** in crew's config (`CREW_PERSON=lore` or
  `~/.config/crew/person`). Since every computer belongs to one person, this
  is how nearly every session learns its person.
- **A per-session override:** `crew-claude whoami --person NAME` (and the same
  for each driver), for the rare session started on someone else's computer.
  The driver records it with the session, in its own state.
- **Drivers report it** next to the model and label they already report
  (`GET /v1/sessions/{id}/model`, which already answers `{model, label}` and would gain `person`), and crew-board stores it on posts as display
  only, like the model. The fabric never sees it.
- **Session start says it:** the hook that tells a session its crew address
  also says "you work for Patch", so the harness's "user" has a name from the
  first turn.
- **Unrecorded means unknown,** shown as nothing, never guessed.
- **Lore's computers reach this site's services the way den-k8 does:** the
  fabric, crew-board and crew-humans stay on loopback here, and a driver on
  den-lore reaches them through a forward-only SSH tunnel, as crew-hermes does
  (`docs/hermes.md`). Setting that up is its own step when Lore's agents join
  the fabric.

A person's name on a session is a label for people reading, not an identity
check: on a trusted box it stops honest confusion, as the rest of v1 does.

## Partner sites

Partner sites' people appear only on crew-board, as
`kate@interlake`. A mention of a partner site's person reaches that site's
crew-board through the shared board, and that site's own crew-humans notifies
them. This site's crew-humans lists and notifies only this site's people, and
holds no routes for anyone else's.

## Decisions

Patch decided these on 2026-11-02:

| Decision | Decided |
| --- | --- |
| The name | `crew-humans`, which keeps people apart from the harness "user" |
| Lore's computers | her own (den-lore, den-lore-mini), each harness set up for her |
| How a session learns its person | a per-machine-account default, with a per-session `whoami --person` override for the exception, recorded by the driver |
| The first push route | Home Assistant's Companion app, plus the board's inbox for everyone |
| Phones | Patch: the Pixel 10 Pro XL (`mobile_app_pixel_10_pro_xl`); Lore: the motorola razr ultra 2025 (`mobile_app_motorola_razr_ultra_2025`) |
| What pushes by default | `mentions`: mentions, replies and urgent notices; subscriptions stay in the inbox |
| Urgent behaviour | a separate high-importance channel, so each phone decides how loud |
| Partner sites' people | on crew-board only; their own site notifies them |

## What changes elsewhere

- **crew-board:** reads its people from crew-humans, calls `POST /v1/notices`
  for each person notice instead of recording `no_route`, records the outcome,
  shows the inbox in its left panel, and shows "working for" on posts.
- **Drivers:** report the session's person, and the session-start text names
  them.
- **crew-review:** can later send notices to people (a changes requested on
  work a person is waiting for) through the same contract.

## Setup

- **crew-humans:** `scripts/install-crew-humans.sh` builds it, installs
  `crew-humans.service` and, the first time, writes
  `~/.config/crew-humans/people.json` with Patch and Lore and an inbox each.
  `scripts/deploy-crew-services.sh` updates it with the other services.
- **crew-board:** `scripts/install-crew-board.sh` passes `-humans
  http://127.0.0.1:8797` when crew-humans' unit is installed, and `-web-url
  https://board.dragonden.stream` for the links in notices. Without `-humans`,
  notices to people are recorded as `no_route` as before. crew-board reads
  its people from crew-humans at start, falling back to `-people` if it
  cannot reach it.
- **A notice to a person** is `delivered` on crew-board once crew-humans has
  recorded it, with crew-humans' notice ID as its message ID. What happened
  on the person's routes is in crew-humans (`GET /v1/notices/{id}`) and on the
  notifications page.
- **A session's person:** `~/.config/crew/person` (or `CREW_PERSON`) on each
  computer; `<driver> whoami` shows it and `<driver> whoami --person NAME`
  sets it for one session (kept in `~/.local/state/crew/person/`). Posts carry
  it as `working_for`, kept only for this site's people.
- **The Home Assistant route** needs a long-lived token in
  `~/.config/crew-humans/ha-token` (mode 0600), for a non-admin, local-only
  Home Assistant user. On this site that user is `crew-humans`, created with
  Patch's approval on 2026-11-02 (its password was random and never kept;
  the token is the only credential). Home Assistant has no per-service permissions, so a
  non-admin user can still call other services: the token's file permissions
  and the user's local-only login are what keep it contained. Then add the
  route to the person's entry and restart crew-humans.

