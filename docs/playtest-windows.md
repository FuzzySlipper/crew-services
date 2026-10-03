# Windows playtest box

Agents test Windows desktop builds of Engine products on a bare-metal Windows
machine (task #8884): products run in native DX12 windows, the playtest
service drives them over the LAN, and the box's one foreground is lent out for
real keyboard and mouse input.

## The box

| Fact | Value |
| --- | --- |
| Name | `den-win11`, Windows 11 Pro (build 26300) |
| Address | `192.168.1.12`. LAN DNS answers every `*.dragonden.stream` name with `192.168.1.6`, so use the address or the SSH alias below. |
| Hardware | Ryzen 7 3700X, 32 GB, RTX 3080 (10 GB) |
| Display | One HDMI output through the user's KVM (EDID name `Ingnok`), 1920x1080@60 |
| Account | Local `agent`, an administrator, no password |

From den-agents:

```bash
ssh den-win11
```

`~/.ssh/config` maps `den-win11` to the address, user `agent` and the key
`~/.ssh/den-w11_ed25519`. The remote shell is Windows PowerShell 5.1. Its
`-Command -` mode reads standard input line by line and breaks multi-line
blocks, so copy scripts over with `scp` and run them with `-File`.

## Setting up a fresh install

1. Create the local `agent` administrator account and sign in once at the console.
2. Install OpenSSH Server (Settings > System > Optional features) and start it.
3. Let den-agents in once to install its key. Windows refuses network logons
   with a blank password, and sshd separately refuses empty passwords, so
   for the bootstrap only:
   - Set the policy "Accounts: Limit local account use of blank passwords to
     console logon only" to Disabled.
   - Make `PermitEmptyPasswords yes` the **first** line of
     `C:\ProgramData\ssh\sshd_config` and restart sshd. It must come before the
     file's closing `Match Group administrators` block.

     ```powershell
     $f="$env:ProgramData\ssh\sshd_config"; Set-Content $f (@('PermitEmptyPasswords yes') + (Get-Content $f)); Restart-Service sshd
     ```

4. From den-agents, copy `scripts/windows/bootstrap.ps1` to the box and run it
   with `$AuthorizedKey` set to `~/.ssh/den-w11_ed25519.pub`. It:
   - installs the key for administrators;
   - makes PowerShell the SSH shell and limits SSH to the local subnet;
   - signs `agent` in at boot;
   - turns off sleep, hibernation, display timeout, the screen saver and the lock screen.
5. Check that key login works, then remove the `PermitEmptyPasswords` line and
   restart sshd. A blank-password SSH login must be refused again.

RDP stays enabled for people. Agents never use it (see below).

## The interactive session

Products, window capture and `SendInput` need the `agent` desktop to be the
**console** session, attached to the 3080's display. Check it with `quser`:
`console` is correct; `rdp-tcp#N` means someone is connected over RDP.

- While RDP is connected, the session renders on the Microsoft Remote Display
  Adapter at the client's resolution, not on the 3080.
- Disconnecting RDP the normal way leaves the session locked and without a
  display. To end an RDP visit without locking, run this in an elevated PowerShell
  inside the RDP session:

  ```powershell
  tscon ((quser $env:USERNAME | Select-Object -Skip 1) -split '\s+')[2] /dest:console
  ```

  It hands the session back to the console. A reboot also works: auto-logon
  signs `agent` in at the console.
- For watching a test, use Sunshine (below), which mirrors the console
  instead of replacing it.

## Display persistence

The KVM keeps the 3080's output attached while it is switched to another
machine. Verified on 2026-10-01: with the KVM switched away, the console stayed
unlocked at 1920x1080@60, and a screen capture from inside the session showed
the live desktop. No dummy plug is needed. Recheck after changing the KVM,
cabling or driver:

```bash
ssh den-win11 'Get-CimInstance -Namespace root\wmi WmiMonitorConnectionParams | Select Active, VideoOutputTechnology; Get-CimInstance Win32_VideoController | Select Name, CurrentHorizontalResolution, CurrentVerticalResolution'
```

These WMI values can lag behind the real state, so the decisive check is a
capture taken inside the console session. If the monitor disappears or the
resolution changes, use the HDMI dummy plug, or a virtual display driver.

## Running something on the desktop

SSH sessions run outside the interactive desktop, so they cannot see windows,
capture the screen or send input. The Windows agent avoids this by starting at
logon inside the session. For one-off desktop work before it exists, register
an interactive scheduled task and start it:

```powershell
$a = New-ScheduledTaskAction -Execute powershell.exe -Argument '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File C:\Users\agent\crew\job.ps1'
$p = New-ScheduledTaskPrincipal -UserId "$env:COMPUTERNAME\agent" -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -Force -TaskName crew-job -Action $a -Principal $p | Out-Null
Start-ScheduledTask crew-job
```

Give the action an absolute path. A failing script reports only an opaque
`LastTaskResult`, so write a transcript from it.

## Two tiers of input

Windows has one foreground window, and OS input (`SendInput`) reaches only
it. So sessions share the box in two tiers:

- **Engine tier (concurrent).** Each session has its own product instance
  and drives it like the `engine` backend does on Linux: live-debug, a
  labelled input claim (`assist act`, `input`) and step-named world captures
  (`world-frame`). None of it needs focus, so several sessions run side by
  side. This tests the product's bindings and simulation, not Windows input.
- **OS tier (one at a time).** `assist {"op":"os-input","steps":[...]}`
  borrows the foreground: the agent leases it to the session, brings the
  window to the front with the cursor over it, sends the steps through
  `SendInput` (keys as scan codes, relative mouse motion, clicks), and ends the
  lease, lifting anything still held. This tests Windows focus, the desktop
  shell's input path, pointer lock and the page's input capture. Click first
  to take a product's pointer lock. A busy foreground answers
  `foreground_busy`; nothing was sent, so try again later.
- `assist {"op":"window"}` captures the window as Windows composes it, world
  and product UI together, without focus.
- **Point at what you saw.** A `point` step's `x`/`y` are a position in the
  image you captured, with its `width` and `height`: the window capture, or
  the desktop capture with `desktop:true` (below).

Receipts carry `tier` and `input_layers`. Engine-tier sessions stay in held
time, so another session holding the foreground for a while changes nothing
for them.

## When something is in the way

If a session's own window and input are not enough (a dialog, another
window in front, a game that never opened its window, an instance stuck at
startup), there are other ways in. Use them in this order.

1. **The whole desktop, through your session.**
   `assist {"op":"window","desktop":true}` captures the full screen: every
   window, dialogs and the taskbar. `assist {"op":"os-input","desktop":true,
   "steps":[...]}` takes the same foreground lease but raises no window, so
   input lands wherever the desktop has it; aim with `point` steps in the
   desktop image, then `click` or `hold` keys:

   ```json
   {"op": "os-input", "desktop": true, "steps": [
     {"kind": "point", "x": 750, "y": 690, "width": 1920, "height": 1080, "ms": 100},
     {"kind": "click", "button": 1, "ms": 80}]}
   ```

2. **The agent's API, without a session.** Anyone on the LAN can read
   `GET http://192.168.1.12:48300/v1/status` (desktop facts, instances,
   the current lease) and `/v1/desktop.png`, and take a desktop lease with
   `POST /v1/lease {"holder": "...", "instance": "", "ttl_ms": 30000}`.
   Stop a stuck instance with `DELETE /v1/instances/{id}`. Leases are shared
   with sessions, so `foreground_busy` means someone else holds the screen.
3. **SSH** (`ssh den-win11`) for anything else: logs under
   `C:\Users\agent\crew\agent-logs`, processes, files, restarting the
   agent (`Stop-ScheduledTask crew-playtest-agent; Start-ScheduledTask
   crew-playtest-agent`) or the box (`Restart-Computer`; it signs back in by
   itself). SSH cannot see or touch the desktop; to run something there, use
   a scheduled task (above).
4. **Sunshine**, for people. It streams the console to a Moonlight client
   without taking it over. The web UI is `https://192.168.1.12:47990`; its
   login is in den-agents' `~/.config/crew-playtest/sunshine-den-win11.txt`.
   Pair a Moonlight client there with the PIN it shows. Its ports are open to
   the local subnet only. Agents use 1 and 2 instead: they need still images
   and discrete input, not a video stream.

**Never RDP.** Windows 11 Pro has one interactive session. An RDP login takes
it over, moves it from the 3080 to a virtual display, and leaves it locked
when it disconnects, which breaks capture and input for every session until
someone runs `tscon` or reboots (see the interactive session, above).

## Profiles

| Profile | Product on the box |
| --- | --- |
| `rusty-rifles-windows` | Rusty Rifles, on the Engine pair it pins; 2 at once |
| `rusty-doom-windows` | Doom's room study, on a runtime pack built at its pin; 3 at once |

```json
{"id": "rusty-rifles-windows", "backend": "engine", "environment": "windows-desktop",
 "windows": {"agent": "http://192.168.1.12:48300", "product": "rusty-rifles"},
 "description": "...", "controls": {}, "reset": "Every start and recover starts a fresh instance."}
```

`start` asks the agent for a fresh instance (its own window, port and lane),
`stop` and `recover` end it, and a failed start stops what it started. The
session's `launch.host` names the instance and its log on the box.

## The Windows agent

`playtest-windows-agent` (`cmd/playtest-windows-agent`) runs at logon inside
the console session; a session-0 service could not open windows, capture
them or send input. It listens on the LAN (`192.168.1.12:48300`), and each
instance serves its product host on the box's LAN address from
48310–48339. The firewall admits both from the local subnet only.

- **Install or update:** build `GOOS=windows go build ./cmd/playtest-windows-agent`,
  copy it with `scripts/windows/install-agent.ps1` and
  `configs/playtest/windows-agent.example.json` to the box, and run the script
  elevated. It keeps an existing `agent.json`, adds the tool paths instances
  need, opens the firewall and registers the `crew-playtest-agent` logon task.
- **Products** in `agent.json` name a checkout and C# project; instances run
  `rusty dev --output window`.
- **Engine versions.** A product runs on the Engine pair its
  `Directory.Build.props` pins: each start runs `rusty install` in its lane
  (a no-op once cached), and `rusty dev` downloads the pair's win-x64
  desktop pack. That needs a pair with win-x64 archives (Engine
  `docs/csharp-distribution.md`). A product's `runtime` instead names a
  runtime pack to run; Doom's room study uses one built at its older pin,
  kept under `C:\Users\agent\crew\runtimes`. `rusty` is
  `C:\Users\agent\crew\bin\rusty.exe`, copied from a pair's runtime pack;
  it hands each command to the product's pinned pair. Neither lives in the
  Engine checkout, which publish builds rewrite.
- **Lanes.** `rusty dev` stages the product into its checkout, and Windows
  cannot replace files a running host holds open, so two instances from one
  checkout collide. A product's `lanes` (3 for Doom) lets that many run at
  once: lane 1 is the checkout, the others are worktrees beside it
  (`C:\dev\rusty-doom-lane2`, ...), moved to the checkout's HEAD and given
  `pnpm install` at each start. A lane's first start builds the product.
- **Restarts.** The agent records its instances and stops leftovers from a
  previous run at startup.
- **API** (JSON): `GET /v1/status`, `POST /v1/instances {product, holder}`,
  `DELETE /v1/instances/{id}`, `GET /v1/instances/{id}/window.png`,
  `GET /v1/desktop.png`, `POST /v1/lease {holder, instance, ttl_ms}` (an
  empty instance leases the desktop), `POST /v1/lease/{id}/input {steps}`,
  `DELETE /v1/lease/{id}`.

## Adding a game

1. **Pin a pair with win-x64 archives.** `rusty status` in the product shows
   its pin; the release's assets show whether it has
   `...-win-x64.tar.gz`. Move an older pin with `rusty update` and run the
   product's own checks on Linux first.
2. **Clone it on the box** (public repositories over HTTPS) into `C:\dev`,
   then run `rusty install` and, when it has `pnpm-lock.yaml`,
   `pnpm install` in it.
3. **Add the product** to `C:\Users\agent\crew\bin\agent.json` (`repo`,
   `project`, `lanes`) and restart the agent:
   `Stop-ScheduledTask crew-playtest-agent; Start-ScheduledTask crew-playtest-agent`.
4. **Add a profile** to den-agents' `~/.config/crew-playtest/games.json` as
   above, then reinstall or restart the playtest service.
5. **Check it:** `playtest start PROFILE`, `assist discover`, an `act` and
   `assist window`, then `stop`.

A lane's first start builds the product, which takes a few minutes.

## Building Engine on the box

The box has the MSVC build tools, Git, Rust (MSVC), .NET 10, Node 22 with
pnpm, cmake, ninja and jq, installed with winget. The Engine checkout is
`C:\dev\rusty-engine`. Builds run in Git Bash inside `vcvars64.bat`'s
environment, with MSVC's directory ahead of Git's own `link` on `PATH`.

- **Runtime packs:** `scripts/build-runtime-pack.sh --desktop` builds
  `target/runtime-pack/win-x64`. A product runs against a pack only when its
  SDK pin is the pack's revision (`CSHARP_PRODUCT_ABI_MISMATCH` otherwise).
- **Published pairs:** Engine's `scripts/publish-windows-pair-packs.sh --host
  den-win11 --checkout C:/dev/rusty-engine` builds a published pair's win-x64
  archives here and adds them to its release (Engine
  `docs/csharp-distribution.md`). The `rusty-windows-pairs` user timer on
  den-agents (`scripts/install-windows-pair-publisher.sh`) runs it with
  `--if-missing` every 30 minutes, so each new Latest pair gets them within
  the hour; `journalctl --user -u rusty-windows-pairs` shows its runs. A
  build takes a few minutes of the box's CPU while sessions run.
- **Gotchas:** PowerShell 5.1 reads BOM-less scripts as ANSI, so keep copied
  scripts ASCII. MSBuild reuse nodes and the Nx daemon outlive a build and keep
  its redirected log open; instances set `MSBUILDDISABLENODEREUSE=1` and
  `NX_DAEMON=false`. `HOME` is set for the agent user, since products' pin
  files find the pair cache under `$(HOME)/.cache/rusty-engine`.
