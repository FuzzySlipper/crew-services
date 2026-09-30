#!/usr/bin/env bash
# Install den-serve from this repository and (re)start its LAN status page.
# The previous binary is kept beside the new one. Running hosts are untouched:
# den-serve state lives in ~/.cache/den-serve and survives the page restart.
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir=${DEN_SERVE_BIN_DIR:-$HOME/.local/bin}
unit_dir=$HOME/.config/systemd/user
mkdir -p "$bin_dir" "$unit_dir"

(cd "$repo_dir" && CGO_ENABLED=0 go build -o "$bin_dir/den-serve.new" ./cmd/den-serve)
if [[ -f "$bin_dir/den-serve" ]]; then
  cp -p "$bin_dir/den-serve" "$bin_dir/den-serve.pre-crew-$(date -u +%Y%m%dT%H%M%SZ)"
fi
mv "$bin_dir/den-serve.new" "$bin_dir/den-serve"

cat > "$unit_dir/den-serve-page.service" <<UNIT
[Unit]
Description=den-serve LAN project status page
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h
Environment=HOME=%h
ExecStart=$bin_dir/den-serve page
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT
# Page-triggered launches need the user's build tools; keep an existing PATH drop-in.
if [[ ! -d "$unit_dir/den-serve-page.service.d" ]]; then
  mkdir -p "$unit_dir/den-serve-page.service.d"
  cat > "$unit_dir/den-serve-page.service.d/tool-path.conf" <<'CONF'
[Service]
# Page-triggered launches need the same user-installed build tools as the CLI.
Environment=PATH=%h/.npm-global/bin:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
CONF
fi
systemctl --user daemon-reload
systemctl --user enable den-serve-page.service
systemctl --user restart den-serve-page.service
