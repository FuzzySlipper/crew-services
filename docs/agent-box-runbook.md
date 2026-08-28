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

For the persistent DSH web setup, save the equivalent user unit as
`~/.config/systemd/user/crew-codex.service`. Keep the instance ID stable and
replace the example mapping with each Codex task that should be projected.

```ini
[Unit]
Description=Codex adapter for the local crew messaging fabric
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Environment=PATH=%h/.npm-global/bin:%h/.local/bin:/usr/local/bin:/usr/bin
ExecStart=%h/.local/bin/crew-codex -fabric-url http://127.0.0.1:8787 -state %h/.local/state/crew-messaging/crew-codex.json -address crew/scout=YOUR_CODEX_THREAD_ID
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

Then run `systemctl --user daemon-reload` and
`systemctl --user enable --now crew-codex.service`. The unit owns its stdio
App Server child; an independently running socket App Server is not reused or
replaced.

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

The adapter does not call `thread/resume`, `turn/steer`, or approval APIs. A
newly created control thread that Codex has not materialized yet uses one
receipt-gated `turn/start` for its first DSH workbench prompt. Once canonical
history is available, ordinary prompts use `thread/queue/add`, which persists
input and lets Codex automatically start it in FIFO order after the thread
becomes idle. Native tool activity, partial messages, reasoning, and approvals
remain outside the projected event surface.

## Codex-backed review service

The separate `crew-review` command owns the local review job ledger and its
ephemeral runtime pool. It calls the current Den MCP endpoint directly for
`request_review`, exact-SHA GitHub gate operations, `get_review_context`, and
`finalize_review`; it does not import Den or Rusty Crew code. Den remains the
review authority, while crew-review owns durable submission admission and the
runtime choice.

Build it beside the messaging binaries and keep its SQLite file separate:

```sh
cd /home/dev/crew-services
go build -o "$HOME/.local/bin/crew-review" ./cmd/crew-review
mkdir -p "$HOME/.local/state/crew-review"
exec "$HOME/.local/bin/crew-review" \
  -listen 127.0.0.1:8413 \
  -db "$HOME/.local/state/crew-review/crew-review.sqlite" \
  -den-mcp-url "${DEN_MCP_URL:-http://192.168.1.10:5199/mcp}" \
  -review-profile "${CREW_REVIEW_PROFILE:-/home/agents/profiles/reviewer/SOUL.md}" \
  -capacity "${CREW_REVIEW_CAPACITY:-2}"
```

The command also accepts `-den-mcp-token`, `-codex-command`, repeated
`-codex-arg`, and `-run-interval`. `DEN_MCP_TOKEN`, `CODEX_COMMAND`,
`CREW_REVIEW_PROFILE`, `CREW_REVIEW_CAPACITY`, and
`CREW_REVIEW_RUN_INTERVAL` are the matching environment settings. Keep the
Codex executable on the service user's `PATH`; if it is installed in a user
bin directory, set that `PATH` in the unit below.

For a persistent agent-box service, save this as
`~/.config/systemd/user/crew-review.service`:

```ini
[Unit]
Description=Codex-backed Crew review runner
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
Environment=PATH=%h/.npm-global/bin:%h/.local/bin:/usr/local/bin:/usr/bin
Environment=DEN_MCP_URL=http://192.168.1.10:5199/mcp
Environment=CREW_REVIEW_PROFILE=/home/agents/profiles/reviewer/SOUL.md
ExecStart=%h/.local/bin/crew-review -listen 127.0.0.1:8413 -db %h/.local/state/crew-review/crew-review.sqlite -capacity 2
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

Then enable it with `systemctl --user daemon-reload` and
`systemctl --user enable --now crew-review.service`. `GET
http://127.0.0.1:8413/healthz` checks SQLite readiness; `GET
http://127.0.0.1:8413/v1/review-pool` reports the configured runtime backend,
queued/running jobs, recent terminal projections, and retained task affinity.
Ephemeral Codex thread IDs are process-local and disappear on service restart;
an in-flight job is requeued when the process context is cancelled so the next
start can run it fresh.

When Den MCP runs on a separate service box, keep `crew-review` loopback-only
and carry its backend connection over SSH. On the agent box, a persistent
reverse tunnel such as the following makes the agent-box listener available as
`127.0.0.1:8413` on `den-srv` without opening a LAN listener:

```ini
[Unit]
Description=Expose the local Crew review runner to Den MCP over SSH
After=network-online.target crew-review.service
Wants=network-online.target crew-review.service

[Service]
ExecStart=/usr/bin/ssh -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R 127.0.0.1:8413:127.0.0.1:8413 den-srv
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

Save that unit as `~/.config/systemd/user/crew-review-den-tunnel.service`, then
enable it after `crew-review.service`. The Den MCP `crew-review` backend remains
`http://127.0.0.1:8413`; from its point of view that address is the remote end
of the tunnel. This topology requires the service account's existing key-only
SSH route to `den-srv` and remote forwarding support.

The Den MCP facade's `submit_task_for_review` tool forwards the public
project/task/repository/exact-SHA/checks/summary envelope to
`POST /v1/review-submissions` on the loopback `crew-review` backend. Configure
that backend in the MCP route/config files before enabling the tool. The
receipt is immediately `gate_pending` when checks are not terminal; retry the
same envelope after Den advances the gate. A passed gate is admitted exactly
once for its Den review round. If the backend is down, the MCP response is a
retryable `den_backend_unavailable` result; the route never falls back to
Rusty or dispatches a second review authority.

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
