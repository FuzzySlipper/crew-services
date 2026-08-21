# Crew messaging agent-box runbook

`crew-messaging` is one trusted-box Go process: JSON/HTTP on a loopback listener plus one SQLite file. It owns the durable directory, immutable messages, deliveries, and rounds; a DSH adapter owns native session activation and insertion. The matching adapter setup is in [dsh-crew](../../dsh-crew/plugins/crew-messaging/docs/agent-box-setup.md).

Build and run it with a user-owned state directory:

```sh
cd /home/dev/crew-services
go build -o "$HOME/.local/bin/crew-messaging" ./cmd/crew-messaging
mkdir -p "$HOME/.local/state/crew-messaging"
exec "$HOME/.local/bin/crew-messaging" \
  -listen 127.0.0.1:8787 \
  -db "$HOME/.local/state/crew-messaging/crew-messaging.sqlite" \
  -lease-duration 5m -ttl 24h
```

The service requires an explicit database path and rejects non-loopback listeners. `GET /healthz` and `GET /readyz` return SQLite readiness. It has no scheduler: a normal adapter poll/restart performs ordinary delivery work, and an operator can explicitly settle abandoned local claims with `POST /v1/maintenance/reap`. Inspection endpoints (`/v1/addresses`, `/v1/messages`, `/v1/deliveries`, `/v1/rounds`, `/v1/traffic`, and mailbox reads) are read-only and never claim, wake, retry, cancel, or dispatch work. `-retention` is currently parsed and validated only; it does not delete data and is omitted here until a retention worker consumes it.

For a persistent desktop session, this minimal user service is equivalent to the foreground command. Save it as `~/.config/systemd/user/crew-messaging.service`; replace the two paths if this checkout or state location differs.

```ini
[Unit]
Description=Local crew messaging fabric

[Service]
ExecStart=%h/.local/bin/crew-messaging -listen 127.0.0.1:8787 -db %h/.local/state/crew-messaging/crew-messaging.sqlite -lease-duration 5m -ttl 24h
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

Install only when desired; this repository does not install or enable it:

```sh
systemctl --user daemon-reload
systemctl --user enable --now crew-messaging.service
systemctl --user status crew-messaging.service
curl --fail http://127.0.0.1:8787/readyz
```

The SQLite file is the durable state. Keep its directory private to the user running the process, stop the service before making a filesystem-level backup, and use a normal replacement start against the same file to recover after a process crash. A pre-dispatch `claimed` delivery may return to `queued` when reaped; a `dispatching` delivery is reconciled only by the owning adapter and otherwise becomes `outcome_unknown`, never silently resent.

Run the proportional local checks from the two sibling repositories:

```sh
cd /home/dev/crew-services && go test ./... && go vet ./...
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsc --noEmit -p ../../plugins/crew-messaging/tsconfig.json
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec vitest run --config ../../plugins/crew-messaging/vitest.config.ts
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsx ../../plugins/crew-messaging/scripts/agent-box-probe.ts
```
