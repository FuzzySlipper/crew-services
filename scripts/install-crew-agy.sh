#!/usr/bin/env bash
# Install or remove crew-agy, the Antigravity CLI (agy) driver for the crew
# messaging fabric: the binary, its user unit, its token file and the crew
# plugin (hooks and skills) in agy's user config.
#
#   scripts/install-crew-agy.sh                 install or update
#   scripts/install-crew-agy.sh --service-only  binary, unit and token only;
#                                               leave ~/.gemini alone
#   scripts/install-crew-agy.sh --profile=FILE  also add the line that starts
#                                               agy with the crew token to a
#                                               shell profile (~/.bashrc, say)
#   scripts/install-crew-agy.sh --uninstall     uninstall the plugin and remove
#                                               the unit (the token file and any
#                                               profile line stay)
#
# agy gives its language server's address and CSRF token only to the commands
# its agent runs, so crew-agy can post into a conversation only when agy was
# started with ANTIGRAVITY_SERVER_CSRF_TOKEN set to the token in
# ~/.config/crew/agy-csrf-token. `eval "$(crew-agy env)"` in a shell profile
# does that. Without it, crew messages still reach a conversation inside its
# turns, through the PreInvocation hook, but never start one.
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
token_file="$HOME/.config/crew/agy-csrf-token"
plugin_dir="$repo_dir/antigravity/plugins/crew"
mode=install
service_only=0
profile=
for arg in "$@"; do
  case "$arg" in
    --uninstall) mode=uninstall ;;
    --service-only) service_only=1 ;;
    --profile=*) profile=${arg#--profile=} ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

# install_plugin installs the crew plugin (hooks.json and the crew-board and
# seat skills) into agy's user config with agy's own installer, which replaces
# an earlier copy. agy loads neither a symlinked plugin folder nor a plugin
# whose files link outside it, and the skills are links to codex/skills, so it
# installs a dereferenced copy. Re-run this script after the plugin or the
# skills change.
install_plugin() {
  local stage
  stage=$(mktemp -d "${TMPDIR:-/tmp}/crew-agy-plugin.XXXXXX")
  cp -rL "$plugin_dir" "$stage/crew"
  agy plugin install "$stage/crew"
  rm -rf -- "$stage"
}

if [[ "$mode" == uninstall ]]; then
  command -v agy >/dev/null 2>&1 && agy plugin uninstall crew || true
  systemctl --user disable --now crew-agy.service 2>/dev/null || true
  rm -f "$unit_dir/crew-agy.service"
  systemctl --user daemon-reload
  echo "crew-agy removed (binary left at $bin_dir/crew-agy; state in ~/.local/state/crew-agy; token at $token_file)"
  exit 0
fi

mkdir -p "$bin_dir" "$unit_dir"
(cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-agy.new" ./cmd/crew-agy)
mv "$bin_dir/crew-agy.new" "$bin_dir/crew-agy"

if [[ ! -s "$token_file" ]]; then
  mkdir -p "$(dirname "$token_file")"
  (umask 077; python3 -I -c 'import secrets; print(secrets.token_hex(24))' > "$token_file")
  echo "made the crew token for agy at $token_file"
fi
chmod 600 "$token_file"

cat > "$unit_dir/crew-agy.service" <<UNIT
[Unit]
Description=Antigravity CLI (agy) driver for the local crew messaging fabric
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Type=simple
ExecStart=$bin_dir/crew-agy serve -fabric http://127.0.0.1:8787 -listen 127.0.0.1:8798
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable crew-agy.service >/dev/null
systemctl --user restart crew-agy.service

if [[ -n "$profile" ]]; then
  line='command -v crew-agy >/dev/null 2>&1 && eval "$(crew-agy env 2>/dev/null)"  # crew-agy: start agy with the crew token'
  if ! grep -qF '# crew-agy: start agy with the crew token' "$profile" 2>/dev/null; then
    printf '\n%s\n' "$line" >> "$profile"
    echo "added the crew token line to $profile"
  fi
fi

if [[ "$service_only" == 1 ]]; then
  echo "crew-agy service installed; run without --service-only to add the crew plugin to agy"
  exit 0
fi
command -v agy >/dev/null 2>&1 || { echo "agy is not on PATH; install Antigravity CLI, then re-run" >&2; exit 1; }
install_plugin
echo "crew-agy installed; the crew plugin applies to agy sessions started from now on"
if [[ -z "$profile" ]]; then
  echo "start agy with the crew token so crew messages can wake it, for example in ~/.bashrc:"
  echo '  eval "$(crew-agy env)"'
fi
