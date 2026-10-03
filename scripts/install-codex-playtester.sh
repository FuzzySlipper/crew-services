#!/usr/bin/env bash
# Install the Codex playtester: link the product-playtest skill and write the
# playtester agent profile. Both use the local crew playtest CLI, so no MCP
# registration is needed. Also retires files the den-services installer left
# for its den-playwright broker, when they still match its ownership record;
# old run evidence under playtester/runs and playtester/state is kept.
set -euo pipefail

usage() {
  echo "usage: ${0##*/} [--check] [--codex-home PATH]" >&2
}

mode=install
codex_root=${CODEX_HOME:-$HOME/.codex}
while (($#)); do
  case $1 in
    --check) mode=check; shift ;;
    --codex-home) codex_root=${2:?--codex-home needs a path}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source_skill=$repo_root/codex/skills/product-playtest
agent_template=$repo_root/codex/agents/playtester.toml.template
installed_skill=$codex_root/skills/product-playtest
installed_agent=$codex_root/agents/playtester.toml
managed_markers='^# Managed by (crew-services|den-services): scripts/install-codex-playtester.sh$'
legacy_dir=$codex_root/playtester
legacy_owner=$legacy_dir/install-owner

fail() { echo "$*" >&2; exit 1; }

check() {
  [[ -L $installed_skill && $(readlink -f "$installed_skill") == "$source_skill" ]] ||
    fail "skill is not linked to $source_skill: $installed_skill"
  [[ $(readlink -f "$codex_root/skills/windows-box") == "$repo_root/codex/skills/windows-box" ]] ||
    fail "windows-box skill is not linked: $codex_root/skills/windows-box"
  cmp -s "$agent_template" "$installed_agent" || fail "agent profile differs from $agent_template: $installed_agent"
  if grep -q '^\[mcp_servers\.den_playtest\]' "$codex_root/config.toml" 2>/dev/null; then
    fail "retired mcp_servers.den_playtest is still registered in $codex_root/config.toml"
  fi
  [[ ! -f $legacy_owner ]] || fail "retired den-playwright install remains: $legacy_owner"
  command -v playtest >/dev/null || fail "playtest CLI is not on PATH; run scripts/install-playtest.sh"
  echo "codex playtester: ok"
}

if [[ $mode == check ]]; then
  check
  exit 0
fi

[[ -f $source_skill/SKILL.md && -f $agent_template ]] || fail "run from a crew-services checkout"
mkdir -p "$codex_root/skills" "$codex_root/agents"

# The skill link may point at the retired den-services copy; nothing else is replaced.
if [[ -L $installed_skill ]]; then
  current=$(readlink "$installed_skill")
  if [[ $(readlink -f "$installed_skill") != "$source_skill" && $current != */den-services/codex/skills/product-playtest ]]; then
    fail "refusing to replace unrelated skill link: $installed_skill -> $current"
  fi
  rm "$installed_skill"
elif [[ -e $installed_skill ]]; then
  fail "refusing to replace non-link skill: $installed_skill"
fi
ln -s "$source_skill" "$installed_skill"

# The Windows test box guide sits beside it.
windows_skill=$codex_root/skills/windows-box
if [[ -e $windows_skill && ! -L $windows_skill ]]; then
  fail "refusing to replace non-link skill: $windows_skill"
fi
ln -sfn "$repo_root/codex/skills/windows-box" "$windows_skill"

if [[ -e $installed_agent ]] && ! head -n 1 "$installed_agent" | grep -Eq "$managed_markers"; then
  fail "refusing to replace unmanaged agent profile: $installed_agent"
fi
install -m 600 "$agent_template" "$installed_agent"

# Retire the den-playwright broker install only where its record still matches.
if [[ -f $legacy_owner ]]; then
  [[ $(head -n 1 "$legacy_owner") == den-services-codex-playtester-v1 ]] || fail "unknown playtester owner record: $legacy_owner"
  if grep -q '^\[mcp_servers\.den_playtest\]' "$codex_root/config.toml" 2>/dev/null; then
    fail "remove the managed den_playtest block from $codex_root/config.toml first"
  fi
  for pair in den-playwright:binary_sha256 den-playwright-x11-input:input_helper_sha256; do
    file=$codex_root/bin/${pair%%:*}
    want=$(sed -n "s/^${pair#*:}=//p" "$legacy_owner")
    if [[ -f $file ]]; then
      [[ $(sha256sum "$file" | cut -d' ' -f1) == "$want" ]] || fail "refusing to remove modified $file"
      rm "$file"
    fi
  done
  rm -f "$legacy_dir/config.yaml" "$legacy_owner"
  echo "retired den-playwright install; kept $legacy_dir/runs and $legacy_dir/state"
fi

check
