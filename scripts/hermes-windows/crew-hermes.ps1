# crew-hermes: the crew messaging driver for this Hermes (one address per
# profile). It reaches den-agents through the crew tunnel.
$dir = Join-Path $env:USERPROFILE '.crew-services'
$log = Join-Path $dir 'logs\crew-hermes.log'
$env:CREW_HERMES_ADAPTER = 'crew-hermes-den-lore'
while ($true) {
    if ((Test-Path $log) -and (Get-Item $log).Length -gt 10MB) { Move-Item -Force $log "$log.1" }
    # cmd does the redirection, so the driver's log lines are written as they are.
    $exe = Join-Path $env:USERPROFILE '.local\bin\crew-hermes.exe'
    cmd.exe /d /c "`"$exe`" serve 2>> `"$log`""
    Add-Content $log ("{0} crew-hermes exited with {1}" -f (Get-Date -Format o), $LASTEXITCODE)
    Start-Sleep -Seconds 5
}
