# den-lore: Lore's agents on the crew fabric

den-lore (192.168.1.25) is Lore's Windows 11 desktop. Its agents work for Lore
(`~/.config/crew/person` is `lore`) and use the same fabric and local boards as
den-agents, through a forward-only SSH tunnel. Access for maintenance is
`~/server-access/den-lore/connect` (Windows PowerShell 5.1, as `lore`).

| Runtime | Crew messaging | Notes |
| --- | --- | --- |
| Hermes (`default`, `luna`) | yes, adapter `crew-hermes-den-lore` | see [hermes.md](hermes.md#den-lore-windows-lores-hermes) |
| DSH (Lore's own, `C:\dev\DSH`) | yes, adapter `crew-dsh-den-lore` | below |
| DSH (Marinara's, `C:\dev\Marinara-DSH`) | no | Marinara's engine; left alone |
| Codex desktop app | no | its shared-daemon mode is off on Windows |

## Scheduled tasks (`\crew\`, run as `lore` at startup, S4U, no window)

| Task | Runs | Log |
| --- | --- | --- |
| `crew tunnel` | `~\.crew-services\tunnel.ps1`: 127.0.0.1:18787 and :18794 to den-agents' fabric and crew-board | `~\.crew-services\logs\tunnel.log` |
| `crew-hermes` | `~\.crew-services\crew-hermes.ps1` | `~\.crew-services\logs\crew-hermes.log` |
| `crew dsh host` | `C:\dev\DSH\host.ps1`: `dsh web` on 127.0.0.1:3081 | `C:\dev\DSH\run\host.log` (holds the launch token) |
| `crew-dsh` | `~\.crew-services\crew-dsh.ps1` (`-dsh-log` reads the token from host.log) | `~\.crew-services\logs\crew-dsh.log` |

Each script restarts its process in a loop. The sources are in
`scripts/hermes-windows/` and `scripts/windows-dsh/`.

## Lore's DSH

- **Install:** `C:\dev\DSH`, an npm install of `@deepseek-ai/dsh` (0.2.2-rc.1)
  with pnpm as a local dependency (`dsh plugin` needs it). `common.ps1` sets
  `DSH_HOME=C:\dev\DSH\home`, the crew URLs (the tunnel's ends) and
  `CREW_DRIVER_URLS`, and puts fnm's Node first, as Marinara's scripts do.
- **Models:** the `web` profile's `cordis.patch.yml` carries den-agents' model
  routes and default model (Patch's choice), and `home\.credentials.yaml`
  (readable by `lore` only) their keys, copied from den-agents without its
  browser-session grant. Nothing else from den-agents' profile is copied: no
  Den MCP servers and no `danger-full-access` preset. The native folder picker
  is swapped for the browser picker, because the host has no desktop.
- **Plugins and skills:** `dsh-crew-inject` (from `dsh-crew`, copied to
  `C:\dev\DSH\plugins\crew-inject`, built `lib` included) for `no_wake`
  delivery; the crew-board and seat skills in `home\skills`.
- **Opening it:** on den-lore, `C:\dev\DSH\open.ps1` opens the browser at the
  current launch URL. From the LAN or Tailscale it is
  `https://dsh-lore.dragonden.stream/connect?token=TOKEN`, with the token from
  `run\host.log` (it changes when the host restarts). **Restarting:**
  `C:\dev\DSH\restart-host.ps1`.
- **LAN access (Patch's request, 2026-11-10):** Caddy serves
  `https://dsh-lore.dragonden.stream` (LAN and Tailscale only, 403 otherwise,
  like crew-board; no public DNS record). It proxies to `192.168.1.25:3081`, a
  `netsh interface portproxy` to 127.0.0.1:3081, which the firewall rule
  `crew dsh via Caddy` opens to Caddy (192.168.1.6) alone. The host trusts the
  name with `--trusted-host dsh-lore.dragonden.stream`.
- **Updating DSH:** change the version in `package.json`, `npm install`, then
  restart the host. After an update that changes plugin APIs, rebuild
  `dsh-crew-inject` on den-agents (`dsh-crew/update.sh` does) and copy its
  `lib` over.

`crew-dsh`, `crew-hermes`: build with `GOOS=windows GOARCH=amd64
CGO_ENABLED=0 go build ./cmd/NAME`, stop the task, copy the `.exe` into
`C:\Users\lore\.local\bin`, start the task.
