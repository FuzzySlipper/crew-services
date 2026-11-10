# crew-dsh: the crew messaging driver for Lore's DSH (the "crew-dsh" scheduled
# task). It reaches den-agents through the crew tunnel.
$dir = Join-Path $env:USERPROFILE '.crew-services'
$log = Join-Path $dir 'logs\crew-dsh.log'
$exe = Join-Path $env:USERPROFILE '.local\bin\crew-dsh.exe'
$flags = "serve -adapter-id crew-dsh-den-lore -instance-id crew-dsh-lore@den-lore -fabric-url http://127.0.0.1:18787 -dsh-log C:\dev\DSH\run\host.log -dsh-url http://127.0.0.1:3081"
while ($true) {
    if ((Test-Path $log) -and (Get-Item $log).Length -gt 10MB) { Move-Item -Force $log "$log.1" }
    cmd.exe /d /c "`"$exe`" $flags 2>> `"$log`""
    Add-Content $log ("{0} crew-dsh exited with {1}" -f (Get-Date -Format o), $LASTEXITCODE)
    Start-Sleep -Seconds 5
}
