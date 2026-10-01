#!/usr/bin/env bash
# Install a user systemd timer that gives each new Latest Engine pair its
# win-x64 archives: every 30 minutes it runs Engine's
# scripts/publish-windows-pair-packs.sh --if-missing against the Windows
# playtest box. See docs/playtest-windows.md.
set -euo pipefail
engine=${RUSTY_ENGINE_CHECKOUT:-$HOME/dev/rusty-engine}
host=${WINDOWS_HOST:-den-win11}
checkout=${WINDOWS_ENGINE_CHECKOUT:-C:/dev/rusty-engine}
[[ -x $engine/scripts/publish-windows-pair-packs.sh ]] || { echo "no Engine checkout at $engine" >&2; exit 1; }
units=$HOME/.config/systemd/user
mkdir -p "$units"
cat > "$units/rusty-windows-pairs.service" <<UNIT
[Unit]
Description=Build and publish win-x64 archives for the Latest Rusty Engine pair
After=network-online.target

[Service]
Type=oneshot
Environment=PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=$engine/scripts/publish-windows-pair-packs.sh --host $host --checkout $checkout --if-missing
TimeoutStartSec=2h
UNIT
cat > "$units/rusty-windows-pairs.timer" <<UNIT
[Unit]
Description=Give each new Latest Rusty Engine pair its win-x64 archives

[Timer]
OnBootSec=10min
OnUnitActiveSec=30min
Persistent=true

[Install]
WantedBy=timers.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now rusty-windows-pairs.timer
systemctl --user list-timers rusty-windows-pairs.timer --no-pager
