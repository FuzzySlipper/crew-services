# Install playtest-windows-agent on the Windows playtest box (see
# docs/playtest-windows.md). Run elevated as the account agents use, with the
# built playtest-windows-agent.exe beside this script. Safe to rerun.
$ErrorActionPreference = 'Stop'
$home_ = $env:USERPROFILE
$bin = "$home_\crew\bin"
New-Item -Force -ItemType Directory $bin, "$home_\crew\agent-logs" | Out-Null

# Stop a running agent (and the instances it started) before replacing it.
Stop-ScheduledTask -TaskName crew-playtest-agent -ErrorAction SilentlyContinue
Get-Process playtest-windows-agent -ErrorAction SilentlyContinue | Stop-Process -Force
Copy-Item -Force "$PSScriptRoot\playtest-windows-agent.exe" "$bin\playtest-windows-agent.exe"
Copy-Item -Force "$PSScriptRoot\run-on-desktop.ps1" "$bin\run-on-desktop.ps1"
if (-not (Test-Path "$bin\agent.json")) {
  Copy-Item "$PSScriptRoot\windows-agent.example.json" "$bin\agent.json"
}

# Instances run rusty dev, which builds product UIs with bash, node and pnpm.
$config = Get-Content "$bin\agent.json" -Raw | ConvertFrom-Json
$path = @("C:\Program Files\Git\bin", "C:\Program Files\nodejs", "$home_\AppData\Local\pnpm", "$home_\AppData\Roaming\npm",
  "C:\Program Files\dotnet", "$home_\.cargo\bin", "$home_\AppData\Local\Microsoft\WinGet\Links") +
  ([Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';') + ([Environment]::GetEnvironmentVariable('Path', 'User') -split ';')
$config.env | Add-Member -Force NoteProperty PATH (($path | Where-Object { $_ } | Select-Object -Unique) -join ';')
$config.env | Add-Member -Force NoteProperty HOME $home_
$config | ConvertTo-Json -Depth 5 | Set-Content -Encoding ascii "$bin\agent.json"

# The agent and its instances serve the LAN only.
$ports = "{0}, {1}-{2}" -f ($config.listen -split ':')[-1], $config.first_port, $config.last_port
Remove-NetFirewallRule -Name crew-playtest-agent -ErrorAction SilentlyContinue
New-NetFirewallRule -Name crew-playtest-agent -DisplayName 'crew playtest agent and instances' -Direction Inbound -Protocol TCP `
  -LocalPort ($ports -split ',\s*') -RemoteAddress LocalSubnet -Action Allow | Out-Null

# At logon, in the interactive session: windows, capture and SendInput need it.
$action = New-ScheduledTaskAction -Execute "$bin\playtest-windows-agent.exe" -WorkingDirectory $bin
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:COMPUTERNAME\$env:USERNAME"
$principal = New-ScheduledTaskPrincipal -UserId "$env:COMPUTERNAME\$env:USERNAME" -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries
Register-ScheduledTask -Force -TaskName crew-playtest-agent -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask crew-playtest-agent
Start-Sleep 2
try { (Invoke-WebRequest -UseBasicParsing "http://$($config.listen)/v1/status").Content } catch { "agent not answering yet: $_" }
