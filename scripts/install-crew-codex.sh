#!/usr/bin/env bash
# Install or update crew-codex, the Codex driver for the crew messaging fabric:
# the binary, its user unit, and its hooks in ~/.codex/hooks.json.
#
#   scripts/install-crew-codex.sh                install or update everything
#   scripts/install-crew-codex.sh --service-only binary and unit only; leave
#                                                ~/.codex/hooks.json alone
#   scripts/install-crew-codex.sh --hooks-only   only add the hooks
#   scripts/install-crew-codex.sh --fill-notify  also fill notify_address on
#                                                submit_task_for_review (only once
#                                                Den accepts the field, #10455)
#   scripts/install-crew-codex.sh --uninstall-hooks  remove the hooks again
#
# The unit keeps the -address pins of an existing crew-codex.service. crew-codex
# connects to the running Codex daemon; this script never starts, stops or
# restarts Codex itself.
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
unit="$unit_dir/crew-codex.service"
hooks_file="${CODEX_HOOKS:-${CODEX_HOME:-$HOME/.codex}/hooks.json}"
service=1
hooks=1
hooks_mode=install
fill_notify=0
for arg in "$@"; do
  case "$arg" in
    --service-only) hooks=0 ;;
    --hooks-only) service=0 ;;
    --fill-notify) fill_notify=1 ;;
    --uninstall-hooks) service=0; hooks_mode=uninstall ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

edit_hooks() {
  # $1: install|uninstall, $2: fill-notify 0|1
  mkdir -p "$(dirname "$hooks_file")"
  [[ -f "$hooks_file" ]] || echo '{"hooks": {}}' > "$hooks_file"
  cp "$hooks_file" "$hooks_file.bak-crew-codex-$(date +%Y%m%d%H%M%S)"
  python3 -I - "$hooks_file" "$bin_dir/crew-codex" "$1" "$2" <<'EOF'
import json, os, sys
path, binary, mode, fill = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1"
with open(path) as f:
    data = json.load(f)
hooks = data.setdefault("hooks", {})

def ours(entry):
    return any("crew-codex hook" in h.get("command", "") for h in entry.get("hooks", []))

for event in list(hooks):
    hooks[event] = [e for e in hooks[event] if not ours(e)]
    if not hooks[event]:
        del hooks[event]

if mode == "install":
    def add(event, name, matcher=None):
        entry = {"hooks": [{"type": "command", "command": f"{binary} hook {name}", "timeout": 3}]}
        if matcher is not None:
            entry["matcher"] = matcher
        hooks.setdefault(event, []).append(entry)
    add("SessionStart", "session-start")
    if fill:
        add("PreToolUse", "fill-notify", matcher="mcp__runner__submit_task_for_review")
with open(path + ".tmp", "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
os.replace(path + ".tmp", path)
EOF
}

if [[ "$service" == 1 ]]; then
  mkdir -p "$bin_dir" "$unit_dir"
  (cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-codex.new" ./cmd/crew-codex)
  mv "$bin_dir/crew-codex.new" "$bin_dir/crew-codex"
  pins=""
  if [[ -f "$unit" ]]; then
    pins=$(grep -o -- '-address [^ ]*' "$unit" | tr '\n' ' ' || true)
  fi
  cat > "$unit" <<EOF
[Unit]
Description=Codex driver for the local crew messaging fabric
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Type=simple
ExecStart=$bin_dir/crew-codex -fabric-url http://127.0.0.1:8787 -control-listen 127.0.0.1:8788 -state $HOME/.local/state/crew-codex/threads.json ${pins}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable crew-codex.service >/dev/null
  systemctl --user restart crew-codex.service
  echo "crew-codex service installed"
fi

if [[ "$hooks" == 1 ]]; then
  edit_hooks "$hooks_mode" "$fill_notify"
  if [[ "$hooks_mode" == install ]]; then
    echo "crew-codex hooks added to $hooks_file; Codex asks to trust new hooks (see /hooks) and runs them in threads started from now on"
  else
    echo "crew-codex hooks removed from $hooks_file"
  fi
fi
