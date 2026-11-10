# Restarts Lore's DSH web host: stops the "crew dsh host" task and the node
# process it started, then starts the task again.
Stop-ScheduledTask -TaskPath '\crew\' -TaskName 'crew dsh host'
Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'node.exe' -and $_.CommandLine -like '*C:\dev\DSH\node_modules*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
Start-Sleep -Seconds 2
$log = 'C:\dev\DSH\run\host.log'
if (Test-Path $log) { Move-Item -Force $log "$log.1" }
Start-ScheduledTask -TaskPath '\crew\' -TaskName 'crew dsh host'
