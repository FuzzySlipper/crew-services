#!/bin/bash
# Managed Wolf browser: navigation is process startup; gameplay stays native input.
set -euo pipefail
: "${PLAYTEST_HOST:?missing game host}"
: "${PLAYTEST_PORT:?missing game port}"
: "${PLAYTEST_URL:?missing loopback game URL}"

forward_args=(--port "$PLAYTEST_PORT" --host "$PLAYTEST_HOST" --target-port "$PLAYTEST_PORT")
if [[ "${PLAYTEST_FORWARD_TRACE:-}" == "1" ]]; then
  forward_args+=(--trace)
fi
/opt/gow/playtest-forward "${forward_args[@]}" &
forward_pid=$!
ready=false
for ((attempt=0; attempt<50; attempt++)); do
  kill -0 "$forward_pid"
  if (exec 3<>"/dev/tcp/127.0.0.1/$PLAYTEST_PORT") 2>/dev/null; then
    ready=true
    break
  fi
  sleep .1
done
"$ready" || { echo "game loopback forwarder did not start" >&2; exit 1; }

exec /usr/games/gamescope -b --force-windows-fullscreen \
  -W "${GAMESCOPE_WIDTH:-1280}" -H "${GAMESCOPE_HEIGHT:-720}" \
  -w "${GAMESCOPE_WIDTH:-1280}" -h "${GAMESCOPE_HEIGHT:-720}" \
  -r "${GAMESCOPE_REFRESH:-30}" -- /usr/bin/firefox --kiosk "$PLAYTEST_URL"
