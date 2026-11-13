# interlake: the first off-site crew site

interlake is an off-site agent box (Bosgame M5, hostname `m5`, Ubuntu 26.04),
run mostly through Hermes and DSH. From den-agents it is `ssh
interlake-agentbox` (Cloudflare Access at ssh.dragonden.party; see
`~/server-access/machine-list.md`). It is its own crew site: its own fabric,
drivers and crew-board, with its agents working for **jb**
(`~/.config/crew/person`). Set up 2026-11-13.

## What runs there (user units of `jb`, lingering)

| Unit | What |
| --- | --- |
| `crew-messaging` | The fabric on 127.0.0.1:8787 (current build; fresh database 2026-11-13, the September one kept as `crew-messaging.sqlite.old-20261113`) |
| `crew-dsh` | DSH driver on 127.0.0.1:8793 (token from the `dsh-web` journal) |
| `crew-hermes` | Hermes driver on 127.0.0.1:8796, one address per profile (`default`, `coder`, `collab`, `researcher`, `reviewer`, `warden-scholar`); the crew plugin is enabled in each |
| `crew-board` | Site `interlake`, people `jb`; API on 127.0.0.1:8794, pages on 0.0.0.0:8795 (LAN and Tailscale only, as everywhere) |
| `dsh-web` | DSH 0.2.2 from `~/system/dsh` on loopback 127.0.0.1:3080, run with the Node 24 in `~/.local/share/dsh-build-tools` (the system Node is 22); drop-in `dsh-web.service.d/runtime.conf` |
| `dsh-web-lan.socket` | 192.168.1.10:3080 to the loopback host (systemd-socket-proxyd), as on den-agents |

`~/.config/crew/env` names the local fabric and board for crew-hermes, whose
defaults are the tunnel ports other Hermes hosts use, and `CREW_DRIVER_URLS`
(dsh and hermes) for `who`; `dsh-web`'s drop-in sets the same for DSH agents.
The crew-board and seat skills are in `~/.dsh/skills` and
`~/.hermes/skills/crew`.

DSH's profile carries `dsh-crew-inject` (from `/data/dev/dsh-crew`) and
`dsh-web-lan-access` 1.4 (`github:AcidGr`, as on den-agents) with its wildcard
webserver row disabled. Update DSH with `/data/dev/dsh-crew/update.sh` (or
`~/.local/bin/update-dsh`) with that Node first on `PATH`.

## Cleanup done 2026-11-13 (Patch's choices)

- Rusty Crew removed: `rusty-crew-m5@a/b` stopped and disabled; the unit,
  `/usr/local/sbin/update-rusty-stack` and `/data/services/rusty-stack` moved
  to `/data/services/.retired/*.20261113`.
- DSH 0.1.6-alpha.2 to 0.2.2-rc.2; the retired in-process
  `dsh-crew-messaging` plugin and the incompatible `dsh-plugin-installer`
  removed; its env file moved to `~/.config/dsh/retired/`. Local edits to the
  retired plugin in `/data/dev/dsh-crew` are in `git stash`.
- Old `crew-codex` and `crew-review` binaries moved to
  `~/.local/share/crew-retired/`.
- Hermes left as it was (v0.22.7, with local changes).

## Shared boards

interlake is meant to join network `hub` (`pixelphantasm/board-hub`). Its
deploy key (`~/.ssh/crew-board-hub`) and signing key
(`~/.ssh/crew-board-signing`) exist, and GitHub's host keys are pinned in its
`known_hosts`. Not joined yet: the pixelphantasm organization has deploy keys
turned off, so the key could not be added.
