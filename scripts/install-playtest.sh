#!/usr/bin/env bash
# Install or update the local playtest service as a user systemd unit.
# Existing games.json and pool.json are preserved. Stop active sessions first.
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
install_dir=${PLAYTEST_INSTALL_DIR:-$HOME/.local/share/crew-playtest}
config_dir=${PLAYTEST_CONFIG_DIR:-$HOME/.config/crew-playtest}
state_dir=${PLAYTEST_STATE_DIR:-$HOME/.local/state/crew-playtest-local}
listen=${PLAYTEST_LISTEN:-127.0.0.1:48200}
# An existing Chromium executable; otherwise Playwright's Chromium is installed.
chromium=${PLAYTEST_CHROMIUM:-}

mkdir -p "$install_dir/bin" "$install_dir/browser" "$config_dir" "$state_dir" "$HOME/.local/bin" "$HOME/.config/systemd/user"
for program in playtest playtest-assist playtest-service; do
  (cd "$repo_dir" && CGO_ENABLED=0 go build -o "$install_dir/bin/$program.new" "./cmd/$program")
  mv "$install_dir/bin/$program.new" "$install_dir/bin/$program"
done
cp "$repo_dir/internal/playtest/scriptworker/worker.mjs" "$install_dir/worker.mjs"
[[ -f "$config_dir/games.json" ]] || cp "$repo_dir/configs/playtest/browser.example.json" "$config_dir/games.json"
[[ -f "$config_dir/pool.json" ]] || printf '{"size": 2}\n' > "$config_dir/pool.json"

# Browser dependencies are owned by this adapter, not the global agent runtime.
cp "$repo_dir/internal/playtest/browser/worker.mjs" "$repo_dir/internal/playtest/browser/playtest.mjs" "$repo_dir/internal/playtest/browser/package.json" "$install_dir/browser/"
(cd "$install_dir/browser" && npm install --omit=dev --no-audit --no-fund)
if [[ -z "$chromium" ]]; then
  (cd "$install_dir/browser" && PLAYWRIGHT_BROWSERS_PATH="$install_dir/browser-binaries" npx playwright install chromium)
  chromium=$(find "$install_dir/browser-binaries" -path '*/chrome-linux64/chrome' -type f | sort | tail -n 1)
  [[ -n "$chromium" ]] || { echo 'Playwright Chromium was not found after installation' >&2; exit 1; }
fi

# Hardware GPU in headless Chromium, for web profiles that use WebGL/WebGPU.
# Engine products render in their own runtime and only need the page to show
# frames. See docs/playtest.md.
cat > "$install_dir/bin/chromium-local.new" <<WRAPPER
#!/bin/bash
set -euo pipefail
features=Vulkan,VulkanFromANGLE,DefaultANGLEVulkan
browser_args=()
for arg in "\$@"; do
  case "\$arg" in
    --enable-features=*) features="\$features,\${arg#--enable-features=}" ;;
    *) browser_args+=("\$arg") ;;
  esac
done
exec $chromium --enable-gpu --use-angle=vulkan --disable-vulkan-surface --enable-unsafe-webgpu "--enable-features=\$features" "\${browser_args[@]}"
WRAPPER
chmod +x "$install_dir/bin/chromium-local.new"
mv "$install_dir/bin/chromium-local.new" "$install_dir/bin/chromium-local"

ln -sfn "$install_dir/bin/playtest" "$HOME/.local/bin/playtest"
ln -sfn "$install_dir/bin/playtest-assist" "$HOME/.local/bin/playtest-assist"
cat > "$HOME/.config/systemd/user/crew-playtest.service" <<UNIT
[Unit]
Description=Local GPU playtest sessions and evidence
After=network.target

[Service]
ExecStart=$install_dir/bin/playtest-service --listen $listen --pool $config_dir/pool.json --games $config_dir/games.json --state $state_dir --worker $install_dir/worker.mjs --browser-worker $install_dir/browser/worker.mjs --chromium $install_dir/bin/chromium-local
Restart=on-failure
RestartSec=2
KillMode=control-group
TimeoutStopSec=50

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable crew-playtest.service
systemctl --user restart crew-playtest.service
# The port sits in Linux's ephemeral range; a client handed it as a source
# port blocks the service's bind (docs/local-services.md).
port=${listen##*:}
if ! tr ',' '\n' < /proc/sys/net/ipv4/ip_local_reserved_ports | grep -qx "$port"; then
  echo "warning: port $port is not in net.ipv4.ip_local_reserved_ports; see docs/local-services.md" >&2
fi
