# Runs Lore's DSH web host on 127.0.0.1:3081 (the "crew dsh host" scheduled
# task); Caddy serves it as https://dsh-lore.dragonden.stream through a port
# proxy on 192.168.1.25:3081 that only Caddy may reach. It restarts the host
# if it exits. Its output, with the launch token, goes to
# run\host.log, which crew-dsh reads.
. (Join-Path $PSScriptRoot 'common.ps1')
New-Item -ItemType Directory -Force -Path $Run | Out-Null
$log = Join-Path $Run 'host.log'
$node = (Get-Command node).Source
while ($true) {
    if ((Test-Path $log) -and (Get-Item $log).Length -gt 10MB) { Move-Item -Force $log "$log.1" }
    cmd.exe /d /c "`"$node`" `"$Dsh`" --profile web --no-open --port $Port --trusted-host dsh-lore.dragonden.stream >> `"$log`" 2>&1"
    Add-Content $log ("{0} dsh web exited with {1}" -f (Get-Date -Format o), $LASTEXITCODE)
    Start-Sleep -Seconds 5
}
