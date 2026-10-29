#!/usr/bin/env bash
# Install or remove crew-claude, the Claude Code driver for the crew messaging
# fabric: the binary, its user unit, and its hooks in ~/.claude/settings.json.
#
#   scripts/install-crew-claude.sh               install or update
#   scripts/install-crew-claude.sh --fill-notify also fill notify_address on
#                                                submit_task_for_review (only once
#                                                Den accepts the field, #10455)
#   scripts/install-crew-claude.sh --service-only  binary and unit only; leave
#                                                ~/.claude/settings.json alone
#   scripts/install-crew-claude.sh --uninstall   remove hooks and the unit
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
settings="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
mode=install
fill_notify=0
service_only=0
for arg in "$@"; do
  case "$arg" in
    --uninstall) mode=uninstall ;;
    --fill-notify) fill_notify=1 ;;
    --service-only) service_only=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

edit_settings() {
  # $1: install|uninstall, $2: fill-notify 0|1
  mkdir -p "$(dirname "$settings")"
  [[ -f "$settings" ]] || echo '{}' > "$settings"
  cp "$settings" "$settings.bak-crew-claude-$(date +%Y%m%d%H%M%S)"
  python3 -I - "$settings" "$bin_dir/crew-claude" "$1" "$2" <<'EOF'
import json, sys
path, binary, mode, fill = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1"
with open(path) as f:
    settings = json.load(f)
hooks = settings.setdefault("hooks", {})

def ours(entry):
    return any("crew-claude hook" in h.get("command", "") for h in entry.get("hooks", []))

for event in list(hooks):
    hooks[event] = [e for e in hooks[event] if not ours(e)]
    if not hooks[event]:
        del hooks[event]

if mode == "install":
    def add(event, name, matcher=None, timeout=3):
        entry = {"hooks": [{"type": "command", "command": f"{binary} hook {name}", "timeout": timeout}]}
        if matcher is not None:
            entry["matcher"] = matcher
        hooks.setdefault(event, []).append(entry)
    add("SessionStart", "session-start")
    add("UserPromptSubmit", "busy")
    add("Stop", "idle")
    # A turn that ends on an API error (expired login, overload) fires
    # StopFailure instead of Stop; without it the session would stay busy.
    add("StopFailure", "idle")
    if fill:
        add("PreToolUse", "fill-notify", matcher="mcp__runner__submit_task_for_review")
    # Messages arrive from crew-claude, not from a child of the session, so the
    # session's inbound default would hold them in bypass-permission sessions.
    settings["crossSessionInbound"] = "accept"
else:
    if settings.get("crossSessionInbound") == "accept":
        del settings["crossSessionInbound"]
if not hooks:
    settings.pop("hooks", None)
with open(path + ".tmp", "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")
import os
os.replace(path + ".tmp", path)
EOF
}

if [[ "$mode" == uninstall ]]; then
  edit_settings uninstall 0
  systemctl --user disable --now crew-claude.service 2>/dev/null || true
  rm -f "$unit_dir/crew-claude.service"
  systemctl --user daemon-reload
  echo "crew-claude removed (binary left at $bin_dir/crew-claude; state in ~/.local/state/crew-claude)"
  exit 0
fi

mkdir -p "$bin_dir" "$unit_dir"
(cd "$repo_dir" && CGO_ENABLED=0 "$(find_go)" build -o "$bin_dir/crew-claude.new" ./cmd/crew-claude)
mv "$bin_dir/crew-claude.new" "$bin_dir/crew-claude"

cat > "$unit_dir/crew-claude.service" <<EOF
[Unit]
Description=Claude Code driver for the local crew messaging fabric
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Type=simple
ExecStart=$bin_dir/crew-claude serve -fabric http://127.0.0.1:8787 -listen 127.0.0.1:8792
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable crew-claude.service >/dev/null
systemctl --user restart crew-claude.service

if [[ "$service_only" == 1 ]]; then
  echo "crew-claude service installed; run without --service-only to add the Claude Code hooks"
  exit 0
fi
edit_settings install "$fill_notify"
echo "crew-claude installed; hooks apply to Claude Code sessions started from now on"
