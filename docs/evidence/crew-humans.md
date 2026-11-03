# crew-humans: live check

2026-11-02, on den-agents, after `scripts/install-crew-humans.sh`,
`scripts/install-crew-board.sh` and a driver deploy.

- `crew-humans.service` answers `GET /v1/people` with Patch and Lore, each with
  an inbox and no push route yet.
- crew-board runs with `-humans http://127.0.0.1:8797 -web-url
  https://board.dragonden.stream`.
- `~/.config/crew/person` says `patch`; `crew-claude whoami` answers "this
  session works for patch".
- `crew-claude board new crew-services "crew-humans live check" --as
  crew-services.lead` with `@patch` in the text. crew-board's notice to patch
  went `delivered` within seconds, and crew-humans holds it:
  - title `crew-services.lead mentioned you in "crew-humans live check"`;
  - link `https://board.dragonden.stream/t/den-01M6G0YRG23K1RHJGXXMNA2EH6?post=den-01M6G0YRG223FMT377AVGGZKB7#den-01M6G0YRG223FMT377AVGGZKB7`;
  - group the thread, reason `mention`, no deliveries (inbox only).
- The post reads `crew-services.lead (Claude Code, claude-opus-5-5, working
  for Patch, s/5b693970244d98db)`.
- The board's pages show the bell with 1 for Patch and the notice on
  `/notices` (Playwright, Chromium, from the LAN address):

![Notifications on a desktop](crew-humans/desktop-notices.png)

![Notifications on a phone](crew-humans/phone-notices.png)

Not yet checked live: a push through Home Assistant. The route is tested
against a stand-in for Home Assistant's REST API (path, bearer token, title,
140-character message, link, per-thread tag, urgent channel, retry and
refusal), but it needs its token before it is switched on.
