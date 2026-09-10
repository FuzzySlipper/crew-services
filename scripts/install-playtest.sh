#!/usr/bin/env bash
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
install_dir=${PLAYTEST_INSTALL_DIR:-/home/system/crew-services/playtest}
state_dir=${PLAYTEST_STATE_DIR:-$HOME/.local/state/crew-playtest}
config_source=${PLAYTEST_MACHINE_CONFIG:-}
mkdir -p "$install_dir/bin" "$state_dir" "$HOME/.local/bin" "$HOME/.config/systemd/user"
for program in playtest playtest-service playtest-forward playtest-target; do
  (cd "$repo_dir" && CGO_ENABLED=0 go build -o "$install_dir/bin/$program.new" "./cmd/$program")
  mv "$install_dir/bin/$program.new" "$install_dir/bin/$program"
done
cp "$repo_dir/internal/playtest/scriptworker/worker.mjs" "$install_dir/worker.mjs"
if [[ ! -f "$install_dir/games.json" ]]; then
  cp "$repo_dir/configs/playtest/games.json" "$install_dir/games.json"
fi
# Browser dependencies are owned by this adapter, not the global agent runtime.
mkdir -p "$install_dir/browser"
cp "$repo_dir/internal/playtest/browser/worker.mjs" "$repo_dir/internal/playtest/browser/package.json" "$install_dir/browser/"
(cd "$install_dir/browser" && npm install --omit=dev --no-audit --no-fund && PLAYWRIGHT_BROWSERS_PATH="$install_dir/browser-binaries" npx playwright install chromium)
if [[ ! -f "$install_dir/machine.json" ]]; then
  if [[ -n "$config_source" ]]; then
    cp "$config_source" "$install_dir/machine.json"
    chmod 600 "$install_dir/machine.json"
  else
    echo 'Installing browser-only service; add a browser profile to games.json. Set PLAYTEST_MACHINE_CONFIG to enable Wolf.' >&2
  fi
fi
wolf_flags=""
if [[ -f "$install_dir/machine.json" ]]; then
  wolf_flags="--config $install_dir/machine.json --forward $install_dir/bin/playtest-forward"
fi
pool_flags=""
if [[ -f "$install_dir/pool.json" ]]; then
  pool_flags="--pool $install_dir/pool.json"
fi
ln -sfn "$install_dir/bin/playtest" "$HOME/.local/bin/playtest"
cat > "$HOME/.config/systemd/user/crew-playtest.service" <<UNIT
[Unit]
Description=Portable agent playtest sessions and JS workers
After=network.target

[Service]
Environment=PLAYWRIGHT_BROWSERS_PATH=$install_dir/browser-binaries
ExecStart=$install_dir/bin/playtest-service $wolf_flags $pool_flags --games $install_dir/games.json --state $state_dir --worker $install_dir/worker.mjs --browser-worker $install_dir/browser/worker.mjs
Restart=on-failure
RestartSec=2
KillMode=control-group
TimeoutStopSec=50

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now crew-playtest.service
systemctl --user restart crew-playtest.service
