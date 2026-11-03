#!/usr/bin/env bash
# Install or update crew-humans, the people on this site and how each is
# notified: the binary, its systemd user unit and, the first time, a people
# file (docs/crew-humans.md).
#
#   scripts/install-crew-humans.sh
#
# crew-humans serves its API on 127.0.0.1:8797. The people file is
# ~/.config/crew-humans/people.json; it is written only when missing, listing
# Patch and Lore with an inbox each. Push routes are added to it by hand
# (docs/crew-humans.md, Routes), then `systemctl --user restart crew-humans`.
# Later updates go through scripts/deploy-crew-services.sh.
set -euo pipefail

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
config_dir="$HOME/.config/crew-humans"
people="$config_dir/people.json"
for arg in "$@"; do
  echo "unknown argument: $arg" >&2
  exit 2
done

mkdir -p "$bin_dir" "$unit_dir"
mkdir -p -m 700 "$config_dir"
(cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-humans.new" ./cmd/crew-humans)
mv "$bin_dir/crew-humans.new" "$bin_dir/crew-humans"
if [[ ! -e "$people" ]]; then
  cat > "$people" <<'PEOPLE'
{
  "site": "den",
  "people": [
    { "name": "patch", "display": "Patch", "routes": [{ "kind": "inbox" }] },
    { "name": "lore", "display": "Lore", "routes": [{ "kind": "inbox" }] }
  ]
}
PEOPLE
  chmod 600 "$people"
fi
cat > "$unit_dir/crew-humans.service" <<UNIT
[Unit]
Description=crew-humans, the people on this site and how each is notified

[Service]
Type=simple
ExecStart=$bin_dir/crew-humans serve -listen 127.0.0.1:8797 -db %h/.local/state/crew-humans/humans.sqlite -people %h/.config/crew-humans/people.json
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable crew-humans.service >/dev/null
systemctl --user restart crew-humans.service
echo "crew-humans installed on 127.0.0.1:8797; people are in $people"
