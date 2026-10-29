#!/usr/bin/env bash
# Build and deploy the crew-services binaries on this box (crew-services #10379).
#
#   scripts/deploy-crew-services.sh                 build, install and restart
#                                                   crew-messaging crew-codex
#                                                   crew-dsh crew-claude
#                                                   crew-review
#   scripts/deploy-crew-services.sh crew-codex ...  only the named services
#   scripts/deploy-crew-services.sh --dry-run       print what would change;
#                                                   builds nothing, restarts
#                                                   nothing
#   scripts/deploy-crew-services.sh --force         restart crew-review even
#                                                   when reviews are in flight
#   scripts/deploy-crew-services.sh --wait=SECONDS  how long to wait for an idle
#                                                   review pool (default 600;
#                                                   0 checks once)
#
# This is the deploy step for crew-services itself. A DeepSeek Harness update
# (dsh-crew/update.sh) no longer rebuilds or restarts any crew service:
# crew-dsh reconnects to a restarted dsh web with its new launch token by
# itself. Deploy from an up-to-date checkout; this script does not pull.
#
# Each old binary is kept as ~/.local/bin/<name>.prev. The resource drop-in for
# crew-review.service (scripts/systemd/crew-review.service.d/) is installed too;
# systemd applies its limits to the running unit on daemon-reload.
#
# crew-review is restarted only when its review pool is idle (queued, running
# and finalizing all 0 at GET /v1/review-pool). If it stays busy for --wait
# seconds, its new binary is not installed and it is reported as skipped, so
# the binary on disk keeps matching the running process. Units that are not
# enabled are installed but not restarted.
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
review_pool_url="${CREW_REVIEW_POOL_URL:-http://127.0.0.1:8413/v1/review-pool}"
dropin_src="$repo_dir/scripts/systemd/crew-review.service.d/50-resources.conf"
dropin_dst="$unit_dir/crew-review.service.d/50-resources.conf"

dry_run=0
force=0
wait_seconds=600
services=()
for arg in "$@"; do
  case "$arg" in
    --dry-run) dry_run=1 ;;
    --force) force=1 ;;
    --wait=*) wait_seconds=${arg#--wait=} ;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    -*) echo "unknown argument: $arg" >&2; exit 2 ;;
    *) services+=("$arg") ;;
  esac
done
[[ "$wait_seconds" =~ ^[0-9]+$ ]] || { echo "--wait needs a number of seconds" >&2; exit 2; }
# Restart order: the fabric first, then its drivers, crew-review last.
(( ${#services[@]} )) || services=(crew-messaging crew-codex crew-dsh crew-claude crew-review)
for s in "${services[@]}"; do
  [[ -d "$repo_dir/cmd/$s" ]] || { echo "no such command: cmd/$s" >&2; exit 2; }
done

run() {
  if (( dry_run )); then printf '[dry-run] %s\n' "$*"; else "$@"; fi
}

# review_pool_idle: 0 when idle, 1 when busy, 2 when the pool cannot be read.
review_pool_idle() {
  local body
  body=$(curl -fsS --max-time 5 "$review_pool_url" 2>/dev/null) || return 2
  python3 -I -c '
import json, sys
p = json.loads(sys.argv[1])
busy = {k: p.get(k, 0) for k in ("queued", "running", "finalizing")}
print("review pool: " + ", ".join(f"{k} {v}" for k, v in busy.items()))
sys.exit(0 if not any(busy.values()) else 1)
' "$body"
}

# wait_for_idle_review_pool: 0 when crew-review may be restarted now.
wait_for_idle_review_pool() {
  if (( force )); then echo "--force: restarting crew-review without checking its review pool"; return 0; fi
  if ! systemctl --user is-active --quiet crew-review.service; then return 0; fi
  local deadline=$((SECONDS + wait_seconds)) status
  while :; do
    status=0
    review_pool_idle || status=$?
    case "$status" in
      0) return 0 ;;
      2) echo "cannot read $review_pool_url; not restarting a running crew-review (use --force)"; return 1 ;;
    esac
    if (( dry_run || SECONDS >= deadline )); then return 1; fi
    sleep 15
  done
}

go_bin=
(( dry_run )) || go_bin=$(find_go)
stage_dir=$(mktemp -d "${TMPDIR:-/tmp}/crew-services-deploy.XXXXXX")
trap 'rm -rf -- "$stage_dir"' EXIT

echo "==> Building ${services[*]} from $repo_dir ($(git -C "$repo_dir" rev-parse --short HEAD 2>/dev/null || echo unknown))"
for s in "${services[@]}"; do
  if (( dry_run )); then
    printf '[dry-run] go build -o %s ./cmd/%s\n' "$stage_dir/$s" "$s"
  else
    (cd "$repo_dir" && CGO_ENABLED=0 "$go_bin" build -o "$stage_dir/$s" "./cmd/$s")
  fi
done

restart_review=1
for s in "${services[@]}"; do
  [[ "$s" == crew-review ]] || continue
  echo "==> Checking the crew-review pool"
  if ! wait_for_idle_review_pool; then
    restart_review=0
    echo "skipping crew-review's binary install and restart; rerun when its pool is idle, or pass --force"
  fi
  # Limits apply on daemon-reload without a restart, so this is safe while busy.
  if ! cmp -s "$dropin_src" "$dropin_dst"; then
    echo "==> Installing the crew-review resource drop-in at $dropin_dst"
    run mkdir -p "$(dirname "$dropin_dst")"
    run install -m 0644 "$dropin_src" "$dropin_dst"
  fi
done

echo "==> Installing binaries into $bin_dir"
run mkdir -p "$bin_dir"
installed=()
for s in "${services[@]}"; do
  if [[ "$s" == crew-review && "$restart_review" == 0 ]]; then continue; fi
  if [[ -f "$bin_dir/$s" ]]; then run cp -p "$bin_dir/$s" "$bin_dir/$s.prev"; fi
  run install -m 0755 "$stage_dir/$s" "$bin_dir/$s"
  installed+=("$s")
done

echo "==> Restarting enabled units"
run systemctl --user daemon-reload
skipped=()
for s in "${installed[@]}"; do
  if systemctl --user is-enabled --quiet "$s.service" 2>/dev/null; then
    echo "restarting $s.service"
    run systemctl --user restart "$s.service"
  else
    echo "$s.service is not enabled: installed, not restarted"
  fi
done
[[ "$restart_review" == 1 ]] || skipped+=(crew-review)

if (( ${#skipped[@]} )); then
  echo "Done; skipped: ${skipped[*]}. Old binaries are kept as <name>.prev in $bin_dir."
  exit 3
fi
echo "Done. Old binaries are kept as <name>.prev in $bin_dir."
