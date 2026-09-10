#!/usr/bin/env bash
# Bootstrap only the dedicated Firefox state used by the remote Wolf runner.
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
managed_preferences=$repo_dir/configs/playtest/firefox/user.js
remote_host=${1:-den-srv}
state_dir=${2:-/data/services/playtest-wolf/playtest/user/WolfFirefox}

if [[ $# -gt 2 ]]; then
	printf 'usage: %s [SSH_HOST] [WOLF_FIREFOX_STATE_DIR]\n' "${0##*/}" >&2
	exit 2
fi
if [[ ! -f "$managed_preferences" ]]; then
	printf 'missing managed preferences: %s\n' "$managed_preferences" >&2
	exit 1
fi
if [[ "$state_dir" != /* ]]; then
	printf 'Wolf Firefox state directory must be absolute: %s\n' "$state_dir" >&2
	exit 2
fi

# The payload is deliberately data, not a shell fragment. It is decoded on the
# remote host as root; the Docker probe uses the existing rootless identity.
managed_encoded=$(base64 < "$managed_preferences" | tr -d '\n')
remote_command=$(printf 'sudo -n bash -s -- %q %q' "$state_dir" "$managed_encoded")

ssh -- "$remote_host" "$remote_command" <<'REMOTE'
set -euo pipefail

state_dir=$1
managed_encoded=$2
firefox_dir=$state_dir/.config/mozilla/firefox
managed_begin='// BEGIN crew-services playtest managed preferences'
managed_end='// END crew-services playtest managed preferences'

docker_rootless() {
	runuser -u docker-rt -- env DOCKER_HOST=unix:///data/services/docker-rt/run/docker.sock docker "$@"
}

# Match the mounted profile state, so configuring a new slot cannot modify a
# live browser and an unrelated active slot does not block this one.
running_containers=$(docker_rootless ps -q)
if [[ -n "$running_containers" ]]; then
	if ! docker_rootless inspect $running_containers | python3 -c '
import json, os, sys
state = os.path.normpath(sys.argv[1])
for container in json.load(sys.stdin):
    if any(os.path.normpath(mount.get("Source", "")) == state for mount in container.get("Mounts", [])):
        raise SystemExit("refusing browser setup while this profile state is mounted by an active container")
' "$state_dir"; then
		exit 3
	fi
fi

if [[ ! -d "$state_dir" ]]; then
	printf 'initialize the dedicated Wolf Firefox state with a first launch before provisioning\n' >&2
	exit 1
fi
mkdir -p "$firefox_dir"
managed_file=$(mktemp "$firefox_dir/.playtest-managed.XXXXXX")
trap 'rm -f "$managed_file"' EXIT
printf '%s' "$managed_encoded" | base64 -d > "$managed_file"

mapfile -d '' prefs_files < <(find "$firefox_dir" -type f -name prefs.js -print0)
if (( ${#prefs_files[@]} == 0 )); then
	if [[ -e "$firefox_dir/profiles.ini" ]]; then
		printf 'refusing to replace existing profiles.ini without a readable prefs.js profile\n' >&2
		exit 1
	fi
	profile_dir=$firefox_dir/playtest.default
	mkdir -p "$profile_dir"
	profile_owner=$(stat -c '%u:%g' "$firefox_dir")
	chown "$profile_owner" "$profile_dir"
	chmod 700 "$profile_dir"
	: > "$profile_dir/prefs.js"
	chown "$profile_owner" "$profile_dir/prefs.js"
	chmod 600 "$profile_dir/prefs.js"
	printf '[General]\nStartWithLastProfile=1\nVersion=2\n\n[Profile0]\nName=playtest\nIsRelative=1\nPath=playtest.default\nDefault=1\n' > "$firefox_dir/profiles.ini"
	chown "$profile_owner" "$firefox_dir/profiles.ini"
	chmod 664 "$firefox_dir/profiles.ini"
	prefs_files=("$profile_dir/prefs.js")
fi

for prefs in "${prefs_files[@]}"; do
	user_js=$(dirname -- "$prefs")/user.js
	if [[ -f "$user_js" ]]; then
		begin_count=$(grep -Fxc "$managed_begin" "$user_js" || true)
		end_count=$(grep -Fxc "$managed_end" "$user_js" || true)
		if [[ "$begin_count" != "$end_count" || "$begin_count" -gt 1 ]]; then
			printf 'refusing malformed managed block in %s\n' "$user_js" >&2
			exit 1
		fi
	else
		begin_count=0
	fi
	temporary=$(mktemp "$(dirname -- "$prefs")/.playtest-user.XXXXXX")
	if [[ "$begin_count" -eq 1 ]]; then
		awk -v begin="$managed_begin" -v end="$managed_end" '
			$0 == begin { skip = 1; next }
			$0 == end { skip = 0; next }
			!skip { print }
		' "$user_js" > "$temporary"
	elif [[ -f "$user_js" ]]; then
		cp "$user_js" "$temporary"
	fi
	{
		printf '\n%s\n' "$managed_begin"
		cat "$managed_file"
		printf '\n%s\n' "$managed_end"
	} >> "$temporary"
	chown 0:0 "$temporary"
	chmod 644 "$temporary"
	mv -f "$temporary" "$user_js"
done

printf 'installed managed Firefox preferences in %d profile(s) under %s\n' "${#prefs_files[@]}" "$firefox_dir"
REMOTE
