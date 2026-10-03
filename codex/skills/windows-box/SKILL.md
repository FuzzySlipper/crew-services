---
name: windows-box
description: Reach and use den-win11, the shared Windows 11 test machine (RTX 3080, DX12), for anything that must run on real Windows - playtesting native desktop builds, building or running an Engine product from the shared checkout at P:, Windows-only bugs, screenshots or clicks on its desktop, or publishing Windows Engine archives. Use when a task says Windows, win-x64, den-win11, the 3080 box, or P:.
---

# Windows box (den-win11)

One bare-metal Windows 11 machine at `192.168.1.12`, shared by every agent.
The full runbook is
[docs/playtest-windows.md](/home/agent/dev/crew-services/docs/playtest-windows.md);
read the section you need before changing anything on the box.

## Pick the route

| You need to | Use |
| --- | --- |
| Playtest a game natively on Windows | The playtest service: `playtest start rusty-rifles-windows` (or `rusty-doom-windows`), then the `product-playtest` skill. Each session gets its own window. |
| Run a command that opens windows or uses `P:` | `ssh den-win11 'C:\Users\agent\crew\bin\run-on-desktop.ps1 -Command "..."'` (runs on the console desktop, waits, returns output and exit code; `-NoWait` for `rusty dev`) |
| Files, logs, processes, restarts | `ssh den-win11` (PowerShell 5.1; key login, already configured) |
| See or click the whole screen | In a session: `assist {"op":"window","desktop":true}` and `{"op":"os-input","desktop":true,...}`. Without one: `GET http://192.168.1.12:48300/v1/desktop.png` and a desktop lease (`POST /v1/lease` with `"instance": ""`). |
| Build from the Linux checkout | `P:` is den-agents' `/home/agent` (`P:\dev\<repo>`), mapped only on the console desktop: use `run-on-desktop.ps1`, not plain SSH. |

## Facts that save time

- `rusty` is on PATH (`C:\Users\agent\.local\bin\rusty.exe`), `HOME` is
  `C:\Users\agent`, and pairs are cached in `C:\Users\agent\.cache\rusty-engine`.
- A product runs on Windows only if its pinned Engine pair has win-x64
  archives (0.1.0-dev.73f9e99ece2e or later; new Latest pairs get them
  automatically within the hour).
- Plain SSH cannot open or see windows and has no saved network login, so
  `P:` there is "Access is denied". That is expected; use the helper.
- Real keyboard/mouse input and desktop leases share one foreground lease.
  `foreground_busy` means another agent holds the screen: wait and retry.
- Copied PowerShell scripts must be ASCII (5.1 reads BOM-less files as ANSI);
  `-Command -` over SSH breaks multi-line blocks, so `scp` a script and run it.

## Do not

- **Never RDP.** It takes over the one console session, moves it off the
  3080 and leaves it locked on disconnect, breaking the box for everyone.
- Do not stop instances, leases or processes another agent owns, or reboot
  while `GET http://192.168.1.12:48300/v1/status` shows instances or a lease.
- Do not change `C:\Users\agent\crew\bin\agent.json`, the Engine checkout at
  `C:\dev\rusty-engine`, or scheduled tasks unless the task is about the box.
