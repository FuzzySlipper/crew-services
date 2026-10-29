#!/usr/bin/env bash
# Install or update crew-dsh, the DeepSeek Harness driver for the crew
# messaging fabric: the binary and its systemd user unit.
#
#   scripts/install-crew-dsh.sh
#
# crew-dsh talks to the running `dsh web` host (dsh-web.service) through its
# session API and reads the host's launch token from that unit's journal. It
# needs no DSH plugin or hook, and this script never starts, stops or restarts
# DSH itself.
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
unit="$unit_dir/crew-dsh.service"
dsh_unit="${DSH_WEB_SERVICE:-dsh-web.service}"
for arg in "$@"; do
  echo "unknown argument: $arg" >&2
  exit 2
done

mkdir -p "$bin_dir" "$unit_dir"
(cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-dsh.new" ./cmd/crew-dsh)
mv "$bin_dir/crew-dsh.new" "$bin_dir/crew-dsh"
cat > "$unit" <<EOF
[Unit]
Description=DeepSeek Harness driver for the local crew messaging fabric
Wants=crew-messaging.service
After=crew-messaging.service $dsh_unit

[Service]
Type=simple
ExecStart=$bin_dir/crew-dsh -fabric-url http://127.0.0.1:8787 -control-listen 127.0.0.1:8793 -dsh-unit $dsh_unit
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable crew-dsh.service >/dev/null
systemctl --user restart crew-dsh.service
echo "crew-dsh service installed; DSH agents can run '$bin_dir/crew-dsh send|who|address'"
