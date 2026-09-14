# Four-slot GPU pool, 2026-09-14 UTC

Expanded the installed pool from two to four while idle. Stopped local admission,
paired isolated Moonlight identities, provisioned remote targets on ports 48192
and 48193, and started crew-playtest.service with size 4. All four GPU sessions
launched concurrently; see allocation.json. Slots 3 and 4 visibly rendered the
Rifles scene (original slot-3.png and slot-4.png). This checks capacity and capture,
not gameplay acceptance or a GPU performance ceiling.

Provisioning initially failed because the new target had no status listener yet.
configure-playtest-pool.sh now handles connection refusal as no running target,
while preserving failures for other network/HTTP/JSON errors and unwrapping the
status result before checking a lease. bash -n and real fresh-slot provisioning
passed. Slot 4 Moonlight pair exited nonzero, but its subsequent isolated app-list
check and actual stream/capture succeeded.

Installed configuration remains /home/system/crew-services/playtest/pool.json
with size 4; private machine/pairing data stays outside Git. All four owned test
sessions were released; final cleanup readback is in cleanup.json.
