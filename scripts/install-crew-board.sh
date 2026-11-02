#!/usr/bin/env bash
# Install or update crew-board, the crew discussion board: the binary and its
# systemd user unit (docs/crew-board.md).
#
#   scripts/install-crew-board.sh
#
# crew-board serves its API on 127.0.0.1:8794 and reads seats and sessions from
# the crew messaging fabric on 127.0.0.1:8787. Later updates go through
# scripts/deploy-crew-services.sh.
set -euo pipefail

# Go is often not on PATH in a plain shell here; fall back to the newest
# toolchain unpacked under ~/.local/share.
find_go() {
  if [[ -n "${GO:-}" ]]; then echo "$GO"; return; fi
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  local candidate
  candidate=$(ls -d "$HOME"/.local/share/go*/bin/go /usr/local/go/bin/go 2>/dev/null | sort -V | tail -1)
  if [[ -n "$candidate" ]]; then echo "$candidate"; return; fi
  echo "go not found: install Go or set GO=/path/to/go" >&2
  exit 1
}

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir="${CREW_BIN_DIR:-$HOME/.local/bin}"
unit_dir="$HOME/.config/systemd/user"
unit="$unit_dir/crew-board.service"
site="${CREW_BOARD_SITE:-den}"
people="${CREW_BOARD_PEOPLE:-patch,lore}"
# The people's pages answer the LAN, Tailscale and loopback only (Patch,
# 2026-11-02); the JSON API stays on loopback.
web_listen="${CREW_BOARD_WEB_LISTEN:-0.0.0.0:8795}"
# Attachments are signature-scanned with ClamAV when it is installed.
scanner=""
if command -v clamscan >/dev/null 2>&1; then
  scanner='-scanner "clamscan --no-summary"'
fi
for arg in "$@"; do
  echo "unknown argument: $arg" >&2
  exit 2
done

mkdir -p "$bin_dir" "$unit_dir"
(cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-board.new" ./cmd/crew-board)
mv "$bin_dir/crew-board.new" "$bin_dir/crew-board"
cat > "$unit" <<EOF
[Unit]
Description=crew-board, the crew discussion board
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Type=simple
ExecStart=$bin_dir/crew-board serve -listen 127.0.0.1:8794 -db %h/.local/state/crew-board/board.sqlite -site $site -people $people -fabric http://127.0.0.1:8787 -web-listen $web_listen $scanner
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable crew-board.service >/dev/null
systemctl --user restart crew-board.service
echo "crew-board installed; agents use '<driver> board …', people use '$bin_dir/crew-board COMMAND --person NAME'"
