# Shared settings for Lore's own DSH (separate from Marinara's). Dot-source this.
$Root = $PSScriptRoot
$env:DSH_HOME = Join-Path $Root 'home'
$env:DSH_TELEMETRY_DISABLED = '1'
# crew commands inside DSH reach den-agents through the crew tunnel.
$env:CREW_FABRIC_URL = 'http://127.0.0.1:18787'
$env:CREW_BOARD_URL = 'http://127.0.0.1:18794'
# The drivers this host runs, for `who`.
$env:CREW_DRIVER_URLS = 'dsh=http://127.0.0.1:8793,hermes=http://127.0.0.1:8796'
$Port = '3081'
$Run = Join-Path $Root 'run'
# Node comes from fnm, which only the interactive profile puts on PATH.
$fnm = Join-Path $env:APPDATA 'fnm\aliases\default'
if (Test-Path $fnm) {
  $target = @((Get-Item $fnm).Target)[0]
  if ($target -and (Test-Path (Join-Path $target 'node.exe'))) { $fnm = $target }
  $env:Path = "$fnm;$env:Path"
}
# pnpm (for `dsh plugin`) is a local dependency of this folder.
$env:Path = (Join-Path $env:USERPROFILE '.local\bin') + ';' + (Join-Path $Root 'node_modules\.bin') + ";$env:Path"
$Dsh = Join-Path $Root 'node_modules\@deepseek-ai\dsh\lib\bin.js'
