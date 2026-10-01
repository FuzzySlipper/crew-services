# Windows playtest box

Agents test Windows desktop builds of Engine products on a bare-metal Windows
machine (task #8884). This page is the machine runbook; the Windows agent and
the playtest service's `windows-desktop` environment are described as they land.

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
- For watching a test, use a viewer that mirrors the console instead of
  replacing it, such as Sunshine and Moonlight. That isn't installed yet.

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
