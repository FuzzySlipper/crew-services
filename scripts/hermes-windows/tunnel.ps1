# crew tunnel: forwards den-lore's 127.0.0.1:18787 (crew messaging) and
# 127.0.0.1:18794 (crew-board) to den-agents with a forward-only key.
$dir = Join-Path $env:USERPROFILE '.crew-services'
$log = Join-Path $dir 'logs\tunnel.log'
while ($true) {
    if ((Test-Path $log) -and (Get-Item $log).Length -gt 5MB) { Move-Item -Force $log "$log.1" }
    Add-Content $log ("{0} starting tunnel" -f (Get-Date -Format o))
    $key = Join-Path $env:USERPROFILE '.ssh\crew-tunnel'
    $ssh = "$env:WINDIR\System32\OpenSSH\ssh.exe"
    cmd.exe /d /c "`"$ssh`" -N -i `"$key`" -o IdentitiesOnly=yes -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o StrictHostKeyChecking=yes -o `"UserKnownHostsFile=$dir\known_hosts`" -L 127.0.0.1:18787:127.0.0.1:8787 -L 127.0.0.1:18794:127.0.0.1:8794 agent@192.168.1.10 2>> `"$log`""
    Add-Content $log ("{0} tunnel exited with {1}" -f (Get-Date -Format o), $LASTEXITCODE)
    Start-Sleep -Seconds 5
}
