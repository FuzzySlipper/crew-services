# Local workstation playtesting

Configured on `den-agents` on 2026-09-27. The RX 9070 XT is available locally
through Mesa/RADV. `crew-playtest.service` runs ten browser slots after the #8692 upgrade. It listens only on `127.0.0.1:48200`. It has no Wolf machine configuration, SSH
target, Moonlight client, or dependency on a remote GPU host.

## Use and configuration

```sh
playtest games
playtest start local-gpu-check
playtest observe SESSION
playtest stop SESSION
systemctl --user status crew-playtest.service
```

- Unit: `/home/agent/.config/systemd/user/crew-playtest.service`
- CLI: `/home/agent/.local/bin/playtest`
- Runtime: `/home/agent/.local/share/crew-playtest/`
- Profiles: `/home/agent/.config/crew-playtest/games.json`
- Pool: `/home/agent/.config/crew-playtest/pool.json`
- Evidence: `/home/agent/.local/state/crew-playtest-local/`

The unit is enabled, and user lingering is enabled. The old installation and
remote profiles under `/home/system/crew-services/playtest` are preserved but
are not the active service configuration. Task #8692 is deployed: the local service and CLI support live configuration reload. The
Chromium distribution is reused from that installation; browser dependencies
were installed with the package lock.

`local-gpu-check` was the initial profile; use `playtest games` for the current
registry. Add actual products using their
local server URL and explicit browser backend:

```json
{
  "id": "my-product",
  "backend": "browser",
  "environment": "service",
  "url": "http://127.0.0.1:3000/",
  "description": "My local product",
  "controls": {"browser": "Product-specific controls"},
  "reset": "Recover creates a fresh browser context; server state may persist."
}
```

Profiles form a JSON array. After changing them or the pool size, run
`playtest reload`; existing sessions continue. Invalid files and shrinks that
would remove occupied slots are refused. See the [reload contract](playtest.md#reload-games-and-pool-configuration). The product's own serving workflow owns its server lifecycle.
Historical LAN URLs and port assignments were not assumed to be valid locally.
Do not run the generic installer over this unit without retaining its local
paths and Chromium launcher: that installer discovers the old Wolf config.

## GPU launch settings

The `--chromium` executable is
`/home/agent/.local/share/crew-playtest/bin/chromium-local`. It launches the
existing full Chromium at
`/home/system/crew-services/playtest/browser-binaries/chromium-1243/chrome-linux64/chrome`
with `--enable-gpu`, `--use-angle=vulkan`, `--disable-vulkan-surface`,
`--enable-unsafe-webgpu`, and the features
`Vulkan,VulkanFromANGLE,DefaultANGLEVulkan`. The wrapper merges Playwright's
own `--enable-features` value so it cannot overwrite those settings.

Default headless launch used SwiftShader here. See Chromium's
[headless GPU guidance](https://chromium.googlesource.com/chromium/src/+/HEAD/docs/gpu/using-gpu-hardware-in-headless-chrome.md)
for the explicit GPU and Vulkan selection. The full combination above was
verified through this service, rather than inferred from GPU presence.

## Verified behavior

- WebGL2 reports `AMD Radeon RX 9070 XT (RADV GFX1201)` through ANGLE/Vulkan.
- WebGPU adapter reports `amd`, `rdna-4`, `isFallbackAdapter: false`; device
  creation succeeds.
- Browser click and scripted Enter input change the visible check result.
- Supervised JavaScript completes and retains checkpoints and an original PNG.
- Two simultaneous sessions receive separate slots.
- A normal service restart closes active test browsers and preserves stopped
  session records; a fresh session succeeds afterward.
- All setup sessions were released, leaving both slots available.

The final script receipt is
`/home/agent/.local/state/crew-playtest-local/local-setup-verification.json`.
Its original capture is
`/home/agent/.local/state/crew-playtest-local/browser/102736eb-e28d-4cfe-884f-4dfcc269541c/a9e4509c-d78c-48a9-8c4d-8cbd82048fdb.png`.

This is infrastructure proof. Product rendering and playability still need
their own observations. The browser backend supports DOM operations, absolute
pointer input, keyboard input, and virtual Gamepad API input. Relative mouse input is supported while the main document holds pointer lock:
`playtest input SESSION --json '[{"kind":"move","dx":40,"dy":-10}]'`.
Use an ordinary click to acquire lock first. Deltas must be integers within
-32767..32767. These are trusted Chromium events, not OS mouse injection.
