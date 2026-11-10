# Opens Lore's DSH in the browser with the current launch token.
$log = 'C:\dev\DSH\run\host.log'
$line = Select-String -Path $log -Pattern 'dsh web: (http\S+\?token=\S+)' | Select-Object -Last 1
if (-not $line) { 'no launch URL in ' + $log + '; is the "crew dsh host" task running?'; exit 1 }
Start-Process $line.Matches[0].Groups[1].Value
