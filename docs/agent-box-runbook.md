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

## Optional Codex session projection

`crew-codex` is a second local process which uses the same fabric endpoint. It
does not replace the native Codex App Server: the adapter owns the managed stdio
connection and canonical-reads each explicit existing native thread with
`thread/read { includeTurns: true }`. It does not require a configured thread
to happen to be on the first page of `thread/list`.

```sh
cd /home/dev/crew-services
go build -o "$HOME/.local/bin/crew-codex" ./cmd/crew-codex
exec "$HOME/.local/bin/crew-codex" \
  -fabric-url http://127.0.0.1:8787 \
  -state "$HOME/.local/state/crew-messaging/crew-codex.json" \
  -address crew/scout=YOUR_CODEX_THREAD_ID
```

Repeat `-address` for each selected thread. The command refuses to map the
same thread twice and will not take over an address bound by another adapter.
It exposes a fabric-owned public session ID and public binding target; the
native thread ID remains only in the adapter-private adoption key. If the
Codex child exits or an RPC fails, the adapter waits for its normal poll delay,
restarts the child, and canonical-rereads history. That replay is safe because
each projected completed message has a stable adapter operation ID.

The optional `-state` file records only successful UI-created threads and their
public mappings, so the same create operation can be replayed and those threads
resume projection after process restart. Keep it in the same user-private state
directory as the SQLite database. Pending approvals and user-input callbacks
remain process-lifetime App Server requests and disappear if its child restarts.

The adapter does not call `thread/resume`, `turn/start`, `turn/steer`, or
approval APIs. For a DSH workbench prompt it uses `thread/queue/add`, which
persists the input and lets Codex automatically start it in FIFO order after
the thread becomes idle. Native tool activity, partial messages, reasoning,
and approvals remain outside the projected event surface.

With an ordinary logged-in local Codex CLI, an opt-in scratch-DB smoke reads one
existing native thread and projects it without mutating Codex:

```sh
cd /home/dev/crew-services
CREW_CODEX_LIVE=1 go test ./internal/codexadapter -run TestLiveCurrentCodexAppServerReadProjection -count=1
```

Run the proportional local checks from the two sibling repositories:

```sh
cd /home/dev/crew-services && go test ./... && go vet ./...
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsc --noEmit -p ../../plugins/crew-messaging/tsconfig.json
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec vitest run --config ../../plugins/crew-messaging/vitest.config.ts
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsx ../../plugins/crew-messaging/scripts/agent-box-probe.ts
```
