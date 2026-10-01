# One-time setup of the Windows playtest box (see docs/playtest-windows.md).
# Run as the account agents use, elevated: an administrator's SSH session is
# elevated already, or use "Run as administrator" at the console. Set
# $AuthorizedKey (or $env:PLAYTEST_AUTHORIZED_KEY) to den-agents' public key
# first. Afterwards the box takes key-based SSH, signs in at boot, and never
# sleeps, blanks or locks. Safe to rerun.
$ErrorActionPreference = 'Stop'

$key = if ($AuthorizedKey) { $AuthorizedKey } else { $env:PLAYTEST_AUTHORIZED_KEY }
if (-not $key -or $key -notmatch '^ssh-ed25519 ') { throw 'set $AuthorizedKey to an ssh-ed25519 public key first' }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'run this from an elevated (Run as administrator) PowerShell'
}
$user = $env:USERNAME

# SSH: OpenSSH server, PowerShell as the remote shell, LAN-only firewall rule.
if ((Get-WindowsCapability -Online -Name 'OpenSSH.Server*').State -ne 'Installed') {
  Add-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0' | Out-Null
}
Set-Service sshd -StartupType Automatic
Start-Service sshd
New-Item -Force -Path 'HKLM:\SOFTWARE\OpenSSH' | Out-Null
Set-ItemProperty 'HKLM:\SOFTWARE\OpenSSH' DefaultShell "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
$rule = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
if (-not $rule) {
  New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Direction Inbound -Protocol TCP -LocalPort 22 -Action Allow | Out-Null
}
Set-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -Enabled True -Profile Any -RemoteAddress LocalSubnet

# The elevated account is an administrator, and sshd reads administrators' keys
# from one machine-wide file that only SYSTEM and Administrators may access.
$keys = "$env:ProgramData\ssh\administrators_authorized_keys"
if (-not (Test-Path $keys) -or -not (Select-String -Quiet -SimpleMatch -Pattern $key -Path $keys)) {
  Add-Content -Encoding ascii $keys $key
}
icacls.exe $keys /inheritance:r /grant 'SYSTEM:F' /grant '*S-1-5-32-544:F' | Out-Null

# Unattended desktop: sign in at boot, never sleep, blank or lock.
$winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
Set-ItemProperty $winlogon AutoAdminLogon '1'
Set-ItemProperty $winlogon DefaultUserName $user
Set-ItemProperty $winlogon DefaultDomainName $env:COMPUTERNAME
if (-not (Get-ItemProperty $winlogon -Name DefaultPassword -ErrorAction SilentlyContinue)) { Set-ItemProperty $winlogon DefaultPassword '' }
foreach ($setting in 'standby-timeout-ac', 'hibernate-timeout-ac', 'monitor-timeout-ac') { powercfg.exe /change $setting 0 }
powercfg.exe /hibernate off
powercfg.exe /setacvalueindex SCHEME_CURRENT SUB_NONE CONSOLELOCK 0
powercfg.exe /setactive SCHEME_CURRENT
Set-ItemProperty 'HKCU:\Control Panel\Desktop' ScreenSaveActive '0'
Set-ItemProperty 'HKCU:\Control Panel\Desktop' ScreenSaverIsSecure '0'
New-Item -Force -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Personalization' | Out-Null
Set-ItemProperty 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Personalization' NoLockScreen 1 -Type DWord

$ip = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.PrefixOrigin -ne 'WellKnown' -and $_.IPAddress -notlike '169.254*' }).IPAddress -join ', '
"ready: ssh $user@$ip  (keys in $keys)"
