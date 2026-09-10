#!/usr/bin/env bash
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
remote_host=${1:-den-srv}
image=${2:-den-playtest-gamescope:local}
build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
(cd "$repo_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$build_dir/playtest-forward" ./cmd/playtest-forward)
cp "$repo_dir/configs/playtest/firefox/"{Dockerfile,startup-app.sh} "$build_dir/"
printf -v remote_command 'sudo -n runuser -u docker-rt -- env DOCKER_HOST=unix:///data/services/docker-rt/run/docker.sock docker build -t %q -' "$image"
tar -C "$build_dir" -cf - . | ssh -- "$remote_host" "$remote_command"
