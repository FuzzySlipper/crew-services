#!/usr/bin/env bash
# Provision isolated Wolf/Moonlight targets for the configured local pool.
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
pool=/home/system/crew-services/playtest/pool.json
legacy_machine=/home/system/crew-services/playtest/machine.json
remote_host=den-srv
check_only=false

usage() {
  printf 'usage: %s [--pool FILE] [--legacy-machine FILE] [--remote HOST] [--check-only]\n' "${0##*/}" >&2
}

while (($#)); do
  case $1 in
    --pool) pool=${2:?--pool needs a file}; shift 2 ;;
    --legacy-machine) legacy_machine=${2:?--legacy-machine needs a file}; shift 2 ;;
    --remote) remote_host=${2:?--remote needs a host}; shift 2 ;;
    --check-only) check_only=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

for program in python3 ssh scp xvfb-run; do
  command -v "$program" >/dev/null || { printf 'missing required command: %s\n' "$program" >&2; exit 1; }
done
[[ -f $pool ]] || { printf 'pool configuration does not exist: %s\n' "$pool" >&2; exit 1; }
[[ -f $legacy_machine ]] || { printf 'legacy machine configuration does not exist: %s\n' "$legacy_machine" >&2; exit 1; }

readarray -t pool_values < <(python3 - "$pool" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    value = json.load(source)
if not isinstance(value, dict) or type(value.get("size")) is not int or not 1 <= value["size"] <= 32:
    raise SystemExit("pool.json size must be an integer in [1, 32]")
wait = value.get("queue_wait_ms")
if wait is not None and (type(wait) is not int or not 0 <= wait <= 20000):
    raise SystemExit("pool.json queue_wait_ms must be an integer in [0, 20000]")
print(value["size"])
PY
)
pool_size=${pool_values[0]}
slots_dir=$(dirname -- "$pool")/slots

if $check_only; then
  python3 - "$legacy_machine" "$slots_dir" "$pool_size" <<'PY'
import json, os, sys
legacy, slots, count = sys.argv[1], sys.argv[2], int(sys.argv[3])
configs = []
for number in range(1, count + 1):
    path = os.path.join(slots, f"slot-{number}", "machine.json")
    if not os.path.isfile(path):
        raise SystemExit(f"missing slot configuration: {path}")
    with open(path) as source:
        value = json.load(source)
    if not isinstance(value, dict):
        raise SystemExit(f"slot configuration is not an object: {path}")
    configs.append(value)
ports = [item.get("target_port") for item in configs]
if len(ports) != len(set(ports)) or any(type(port) is not int or not 1 <= port <= 65535 for port in ports):
    raise SystemExit("slot target_port values must be unique valid ports")
if configs[0] != json.load(open(legacy)):
    raise SystemExit("slot 1 must preserve the legacy machine configuration")
if len({item.get("state") for item in configs}) != len(configs):
    raise SystemExit("slot capture state directories must be distinct")
if len({item.get("moonlight_config") for item in configs}) != len(configs):
    raise SystemExit("slot Moonlight configuration directories must be distinct")
print(f"pool configuration is valid for {count} slots")
PY
  exit 0
fi

mkdir -p "$slots_dir"
chmod 700 "$slots_dir"
python3 - "$legacy_machine" "$slots_dir" "$pool_size" <<'PY'
import json, os, stat, sys, tempfile
legacy_path, slots, count = sys.argv[1], sys.argv[2], int(sys.argv[3])
with open(legacy_path) as source:
    legacy = json.load(source)
if not isinstance(legacy, dict):
    raise SystemExit("legacy machine configuration is not an object")
required = ("ssh_host", "target_port", "stream_host", "moonlight", "moonlight_config", "state")
if any(not legacy.get(key) for key in required):
    raise SystemExit("legacy machine configuration lacks required Wolf fields")
for number in range(1, count + 1):
    directory = os.path.join(slots, f"slot-{number}")
    path = os.path.join(directory, "machine.json")
    os.makedirs(directory, mode=0o700, exist_ok=True)
    os.chmod(directory, 0o700)
    if os.path.exists(path):
        continue
    config = dict(legacy)
    if number > 1:
        config["target_port"] = int(legacy["target_port"]) + number - 1
        config["state"] = os.path.join(str(legacy["state"]), "slots", f"slot-{number}")
        config["moonlight_config"] = os.path.join(str(legacy["moonlight_config"]), "slots", f"slot-{number}")
    fd, temporary = tempfile.mkstemp(prefix="machine.", dir=directory)
    with os.fdopen(fd, "w") as output:
        json.dump(config, output, indent=2, sort_keys=True)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    os.chmod(temporary, stat.S_IRUSR | stat.S_IWUSR)
    os.replace(temporary, path)
PY

"$0" --pool "$pool" --legacy-machine "$legacy_machine" --remote "$remote_host" --check-only

needs_pairing=false
for number in $(seq 2 "$pool_size"); do
  if ! ssh -- "$remote_host" "sudo -n test -s /data/services/playtest-wolf/controller/slots/slot-$number/client-id"; then
    needs_pairing=true
    break
  fi
done

# A fresh Moonlight pairing must not disturb an existing stream. Once a slot
# already has its own paired identity, creating its state and target unit does
# not restart or modify another slot's stream.
ssh -- "$remote_host" "sudo -n env NEEDS_PAIRING=$needs_pairing python3 -" <<'PY'
import json, os, subprocess, sys
socket = "/data/services/playtest-wolf/wolf.sock"
def call(route, data=None):
    command = ["curl", "-fsS", "--unix-socket", socket, "http://wolf/api/v1/" + route]
    if data is not None:
        command += ["-H", "Content-Type: application/json", "--data-binary", json.dumps(data)]
    return json.loads(subprocess.check_output(command))
sessions = call("sessions").get("sessions", [])
pending = call("pair/pending").get("requests", [])
if os.environ["NEEDS_PAIRING"] == "true" and sessions:
    raise SystemExit("refusing pool provisioning while Wolf has a stream session")
if pending:
    raise SystemExit("refusing pool provisioning while Wolf pairing is pending")
for port in range(48190, 48222) if os.environ["NEEDS_PAIRING"] == "true" else []:
    try:
        response = subprocess.check_output(["curl", "-fsS", "--max-time", "2", "-H", "Content-Type: application/json", "--data-binary", json.dumps({"op": "status"}), f"http://127.0.0.1:{port}/command"], stderr=subprocess.DEVNULL)
        result = json.loads(response).get("result", {})
        if result.get("lease"):
            raise SystemExit(f"refusing pool provisioning while target port {port} has a lease")
    except subprocess.CalledProcessError:
        pass
PY

# A later pool-size increase reuses the installed target. Only the initial
# upgrade that introduces the isolation flags compiles and deploys this binary.
if ! ssh -- "$remote_host" '/data/services/playtest-wolf/controller/playtest-target -h 2>&1 | grep -q runner-state-folder && /data/services/playtest-wolf/controller/playtest-target -h 2>&1 | grep -q wolf-state-dir'; then
  command -v go >/dev/null || { printf 'Go is required to install the isolation-capable target binary\n' >&2; exit 1; }
  build_dir=$(mktemp -d)
  trap 'rm -rf "$build_dir"' EXIT
  (cd "$repo_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$build_dir/playtest-target" ./cmd/playtest-target)
  scp -q "$build_dir/playtest-target" "$remote_host:/tmp/crew-playtest-target.$$.new"
  ssh -- "$remote_host" "sudo -n install -o docker-rt -g den-pi-docker -m 0755 /tmp/crew-playtest-target.$$.new /data/services/playtest-wolf/controller/playtest-target && rm -f /tmp/crew-playtest-target.$$.new"
fi

machine_value() {
  python3 - "$1" "$2" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    value = json.load(source)[sys.argv[2]]
if not isinstance(value, (str, int)):
    raise SystemExit("machine value has an unsupported type")
print(value)
PY
}

# Slot 1 retains the deployed legacy controller service and its original paired
# identity/capture state. Extend its Go drop-in in place rather than replacing
# that service or copying any credentials into pool configuration.
legacy_port=$(machine_value "$legacy_machine" target_port)
legacy_dropin=/etc/systemd/system/den-playtest-controller.service.d/go.conf
legacy_has_input=$(ssh -- "$remote_host" "sudo -n grep -q -- --wolf-state-dir $legacy_dropin && echo yes || true")
if [[ $legacy_has_input != yes ]]; then
  legacy_busy=$(ssh -- "$remote_host" "curl -fsS --max-time 3 -H 'Content-Type: application/json' --data-binary '{\"op\":\"status\"}' http://127.0.0.1:$legacy_port/command | python3 -c 'import json, sys; d=json.load(sys.stdin); print(1 if d.get(\"result\", d).get(\"lease\") else 0)'")
  if [[ $legacy_busy != 0 ]]; then
    printf 'legacy target unit lacks input isolation while slot 1 has an active lease; release slot 1 before applying it\n' >&2
    exit 1
  fi
  ssh -- "$remote_host" "sudo -n python3 - $legacy_dropin" <<'PY'
import os, sys, tempfile
path = sys.argv[1]
with open(path) as source:
    lines = source.readlines()
for index in range(len(lines) - 1, -1, -1):
    line = lines[index]
    if line.startswith('ExecStart=') and 'playtest-target' in line:
        if '--wolf-state-dir' not in line:
            lines[index] = line.rstrip('\n') + ' --runner-state-folder playtest/user/WolfFirefox --runner-container-name WolfFirefox --wolf-state-dir /data/services/playtest-wolf\n'
        break
else:
    raise SystemExit('legacy Go target ExecStart was not found')
directory = os.path.dirname(path)
fd, temporary = tempfile.mkstemp(prefix='.go.conf.', dir=directory)
try:
    with os.fdopen(fd, 'w') as output:
        output.writelines(lines)
        output.flush()
        os.fsync(output.fileno())
    os.chmod(temporary, 0o644)
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
  ssh -- "$remote_host" "sudo -n systemctl daemon-reload && sudo -n systemctl restart den-playtest-controller.service"
fi

for number in $(seq 2 "$pool_size"); do
  slot_dir=$slots_dir/slot-$number
  machine=$slot_dir/machine.json
  target_port=$(machine_value "$machine" target_port)
  stream_host=$(machine_value "$machine" stream_host)
  moonlight=$(machine_value "$machine" moonlight)
  moonlight_config=$(machine_value "$machine" moonlight_config)
  remote_slot=/data/services/playtest-wolf/controller/slots/slot-$number
  client_id_file=$remote_slot/client-id
  state_folder=playtest/user/WolfFirefox-slot-$number
  container_name=WolfFirefox-slot-$number
  browser_owner=$(ssh -- "$remote_host" 'sudo -n stat -c "%u:%g" /data/services/playtest-wolf/playtest/user/WolfFirefox')
  browser_uid=${browser_owner%%:*}
  browser_gid=${browser_owner##*:}

  mkdir -p "$moonlight_config" "$slot_dir"
  chmod 700 "$moonlight_config" "$slot_dir"
  if ! ssh -- "$remote_host" "sudo -n test -s $client_id_file"; then
    before_clients=$(ssh -- "$remote_host" 'sudo -n curl -fsS --unix-socket /data/services/playtest-wolf/wolf.sock http://wolf/api/v1/clients')
    random_pin=$(od -An -N2 -tu2 /dev/urandom | tr -d '[:space:]')
    printf -v pin '%04d' "$((random_pin % 10000))"
    pair_log=$slot_dir/moonlight-pair.log
    umask 077
    XDG_CONFIG_HOME="$moonlight_config" xvfb-run -a timeout 60 "$moonlight" pair --pin "$pin" "$stream_host" >"$pair_log" 2>&1 &
    pair_pid=$!
    pair_secret=""
    for _ in $(seq 1 100); do
      pending=$(ssh -- "$remote_host" 'sudo -n curl -fsS --unix-socket /data/services/playtest-wolf/wolf.sock http://wolf/api/v1/pair/pending')
      pair_secret=$(python3 - "$pending" <<'PY'
import json, sys
requests = json.loads(sys.argv[1]).get("requests", [])
if len(requests) == 1:
    print(requests[0]["pair_secret"])
elif len(requests) > 1:
    raise SystemExit("more than one Wolf pairing request is pending")
PY
) || { kill "$pair_pid" 2>/dev/null || true; wait "$pair_pid" 2>/dev/null || true; exit 1; }
      [[ -n $pair_secret ]] && break
      sleep .2
    done
    [[ -n $pair_secret ]] || { kill "$pair_pid" 2>/dev/null || true; wait "$pair_pid" 2>/dev/null || true; printf 'Moonlight did not create a pairing request; inspect its private slot log\n' >&2; exit 1; }
    printf '{"pair_secret":%s,"pin":%s}' "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$pair_secret")" "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$pin")" |
      ssh -- "$remote_host" 'sudo -n curl -fsS --unix-socket /data/services/playtest-wolf/wolf.sock -H "Content-Type: application/json" --data-binary @- http://wolf/api/v1/pair/client' >/dev/null
    pair_status=0
    wait "$pair_pid" || pair_status=$?
    # A newly registered Wolf identity alone is not enough: verify that the
    # isolated local Moonlight state can use it to enumerate Wolf's apps. This
    # is read-only and never starts a stream. Some Moonlight builds report a
    # nonzero exit while tearing down their temporary pair window, so a
    # successful list is the explicit completion condition in that case.
    if ! XDG_CONFIG_HOME="$moonlight_config" xvfb-run -a timeout 20 "$moonlight" list "$stream_host" >>"$pair_log" 2>&1; then
      printf 'Moonlight pairing did not produce a usable local identity; inspect its private slot log\n' >&2
      exit 1
    fi
    if ((pair_status != 0)); then
      printf 'Moonlight pair exited nonzero but the isolated app-list verification succeeded\n' >&2
    fi
    after_clients=$(ssh -- "$remote_host" 'sudo -n curl -fsS --unix-socket /data/services/playtest-wolf/wolf.sock http://wolf/api/v1/clients')
    client_id=$(python3 - "$before_clients" "$after_clients" <<'PY'
import json, sys
before = {item["client_id"] for item in json.loads(sys.argv[1]).get("clients", [])}
after = {item["client_id"] for item in json.loads(sys.argv[2]).get("clients", [])}
created = after - before
if len(created) != 1:
    raise SystemExit("Moonlight pairing did not add exactly one new Wolf identity")
print(created.pop())
PY
)
    printf '%s\n' "$client_id" | ssh -- "$remote_host" "sudo -n install -D -o docker-rt -g den-pi-docker -m 0600 /dev/stdin $client_id_file"
  fi

  ssh -- "$remote_host" "sudo -n install -d -o docker-rt -g den-pi-docker -m 0750 $remote_slot && sudo -n install -d -o $browser_uid -g $browser_gid -m 0755 /data/services/playtest-wolf/$state_folder"
  "$repo_dir/scripts/setup-playtest-browser.sh" "$remote_host" "/data/services/playtest-wolf/$state_folder"
  ssh -- "$remote_host" "sudo -n chown -R $browser_owner /data/services/playtest-wolf/$state_folder"
  client_id=$(ssh -- "$remote_host" "sudo -n cat $client_id_file")
  unit=$(printf '%s\n' \
    '[Unit]' \
    "Description=Wolf agent playtest target slot $number" \
    'After=network.target' \
    '' \
    '[Service]' \
    'Type=simple' \
    'User=docker-rt' \
    'Group=den-pi-docker' \
    "ExecStart=/data/services/playtest-wolf/controller/playtest-target --socket /data/services/playtest-wolf/wolf.sock --state $remote_slot/state --client-id $client_id --port $target_port --runner-state-folder $state_folder --runner-container-name $container_name --wolf-state-dir /data/services/playtest-wolf --video-producer-buffer-caps video/x-raw(memory:DMABuf),drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}" \
    'Restart=on-failure' \
    'RestartSec=3' \
    'TimeoutStopSec=40' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target')
  unit_path=/etc/systemd/system/den-playtest-target@$number.service
  unit_hash=$(printf '%s\n' "$unit" | sha256sum | awk '{print $1}')
  remote_hash=$(ssh -- "$remote_host" "if sudo -n test -f $unit_path; then sudo -n sha256sum $unit_path | awk '{print \$1}'; fi")
  unit_changed=0
  if [[ $remote_hash != "$unit_hash" ]]; then
    slot_busy=$(ssh -- "$remote_host" "curl -fsS --max-time 3 -H 'Content-Type: application/json' --data-binary '{\"op\":\"status\"}' http://127.0.0.1:$target_port/command | python3 -c 'import json, sys; print(1 if json.load(sys.stdin).get(\"lease\") else 0)'")
    if [[ $slot_busy != 0 ]]; then
      printf 'target unit differs while slot %s has an active lease; release that slot before applying its configuration\n' "$number" >&2
      exit 1
    fi
    printf '%s\n' "$unit" | ssh -- "$remote_host" "sudo -n install -D -o root -g root -m 0644 /dev/stdin $unit_path"
    ssh -- "$remote_host" "sudo -n systemctl daemon-reload"
    unit_changed=1
  fi
  if (( unit_changed )); then
    ssh -- "$remote_host" "sudo -n systemctl restart den-playtest-target@$number.service"
  else
    ssh -- "$remote_host" "sudo -n systemctl enable --now den-playtest-target@$number.service"
  fi
  ssh -- "$remote_host" "curl -fsS --max-time 3 -H 'Content-Type: application/json' --data-binary '{\"op\":\"status\"}' http://127.0.0.1:$target_port/command >/dev/null"
done

printf 'provisioned playtest pool with %s isolated slot(s)\n' "$pool_size"
