# Run a PowerShell command in agent's interactive console session and wait
# for it. SSH sessions cannot open windows or use the desktop's saved network
# logins (such as P:); a command run this way can. Prints the command's
# output and exits with its exit code. Keep this file ASCII: Windows
# PowerShell 5.1 reads BOM-less scripts as ANSI.
#
#   run-on-desktop.ps1 -Command 'Get-PSDrive P; rusty status' [-TimeoutSec 600] [-NoWait]
#
# -NoWait starts the command and returns its log path, for something that
# keeps running (rusty dev); stop it with Stop-Process or by its own means.
param(
    [Parameter(Mandatory = $true)][string]$Command,
    [int]$TimeoutSec = 600,
    [switch]$NoWait
)
$ErrorActionPreference = 'Stop'
$id = 'crew-desktop-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$dir = Join-Path $env:USERPROFILE 'crew\desktop-runs'
New-Item -ItemType Directory -Force $dir | Out-Null
$script = Join-Path $dir "$id.ps1"
$log = Join-Path $dir "$id.log"
$status = Join-Path $dir "$id.exit"
# The wrapper refreshes PATH from the registry, as a new PowerShell window would.
$body = @"
`$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
`$code = 0
try {
    & { $Command } *>&1 | Out-File -Encoding utf8 -FilePath '$log'
    if (`$LASTEXITCODE) { `$code = `$LASTEXITCODE }
} catch {
    `$_ | Out-File -Encoding utf8 -Append -FilePath '$log'
    `$code = 1
}
Set-Content -Encoding ascii -Path '$status' -Value `$code
Unregister-ScheduledTask -TaskName '$id' -Confirm:`$false -ErrorAction SilentlyContinue
"@
Set-Content -Encoding utf8 -Path $script -Value $body
$action = New-ScheduledTaskAction -Execute powershell.exe -Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$script`""
$principal = New-ScheduledTaskPrincipal -UserId "$env:COMPUTERNAME\$env:USERNAME" -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -Force -TaskName $id -Action $action -Principal $principal | Out-Null
Start-ScheduledTask -TaskName $id
if ($NoWait) {
    Write-Output "started $id; log $log"
    exit 0
}
$deadline = (Get-Date).AddSeconds($TimeoutSec)
while (-not (Test-Path $status) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
Unregister-ScheduledTask -TaskName $id -Confirm:$false -ErrorAction SilentlyContinue
if (Test-Path $log) { Get-Content $log }
if (-not (Test-Path $status)) {
    Write-Error "timed out after $TimeoutSec s; the command may still be running (log $log)"
    exit 124
}
exit [int](Get-Content $status)
