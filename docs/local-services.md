# Local-machine services

crew-services owns the agent services that run on the agent box (`den-agents`)
itself. den-services keeps the Den services deployed on the LAN host (Proxmox
CT106, `192.168.1.5`; see its `deployment/services.yaml`) and their client.
This split was made on 2026-09-29 (task #8883); before it, several local tools
lived in den-services only for discoverability.

## What runs here

| Component | Where | Installed as |
| --- | --- | --- |
| Agent playtesting | `cmd/playtest`, `cmd/playtest-service`, `internal/playtest` | `crew-playtest.service`, `~/.local/bin/playtest` ([guide](playtest.md)) |
| Bounded Jev assistant (research) | `cmd/playtest-assist`, `internal/playtest/assistant` | `~/.local/bin/playtest-assist` ([guide](playtest-assistant.md)) |
| Dev/demo hosts and LAN status page | `cmd/den-serve`, `internal/serve`, `internal/devserver` | `~/.local/bin/den-serve`, `den-serve-page.service` ([guide](den-serve.md)) |
| Codex playtester skill and agent profile | `codex/skills/product-playtest`, `codex/agents/playtester.toml.template` | `scripts/install-codex-playtester.sh` links and writes them into `~/.codex` |
| Messaging, review and Codex adapters | `cmd/crew-messaging`, `cmd/crew-review`, `cmd/crew-codex` | their own user units |

The RX 9070 XT's power is capped at 250 W (default 317 W) by
`/etc/udev/rules.d/90-den-agents-rx9070xt-power.rules`, since this machine
shares a UPS; see [playtest](playtest.md#local-service).

Windows desktop builds are tested on a separate bare-metal box, `den-win11`
([runbook](playtest-windows.md)); the playtest service reaches its agent from
here. The `rusty-windows-pairs` user timer builds win-x64 archives on it for
each new Latest Engine pair.

The playtest API listens on `127.0.0.1:48200`, inside Linux's ephemeral port
range. A client that polls another local service can be handed 48200 as its
source port, and its TIME-WAIT then blocks the playtest service from binding
on restart. `/etc/sysctl.d/60-crew-services-ports.conf` reserves it
(`net.ipv4.ip_local_reserved_ports = 48200`); den-serve's instance range
(30300–30450) is below the ephemeral range.

The playtest service starts a private product host per hosted session through
den-serve's broker, so both share one port registry and one status page.

## Decisions from the den-services inventory

| den-services component | Decision |
| --- | --- |
| `den-serve`, `devserver-broker` | Moved here, with their last working tree. Binary name unchanged. |
| `playwright-broker` (`den-playwright run/playtest/mcp`, `playtest-x11-input`) | Retired. Its persistent playtest mode duplicated crew playtest, and nothing used `den-playwright run`. Repositories' `.den-playwright.json` files keep working as den-serve manifests. Its permissive raw-CDP tier can be recovered from den-services history if a need appears. |
| `codex/skills/product-playtest`, playtester agent template | Moved here; the skill describes this service. |
| `codex/playtester` config, `install-codex-playtester.sh`, adoption check | Retired with the broker. The crew installer removes the broker's binaries and config when they still match its ownership record, and keeps old run evidence (`~/.codex/playtester/runs`, which can be large). |
| `cmd/den-tool` and its `den-tool-cli` skill | Stay in den-services: it is the client of those services' API and is generated from their MCP catalog. |
| `migration`, `integration`, `shared` | Stay: deploy-time and shared libraries of the deployed services. |
| Deployed services (web-edge, gateway, runtime, …) | Stay. |
