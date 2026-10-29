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

The service requires an explicit database path and rejects non-loopback listeners. `GET /healthz` and `GET /readyz` return SQLite readiness. It has no scheduler: a normal adapter poll/restart performs ordinary delivery work, and an operator can explicitly settle abandoned local claims with `POST /v1/maintenance/reap`. Inspection endpoints (`/v1/addresses`, `/v1/messages`, `/v1/deliveries`, `/v1/rounds`, `/v1/traffic`, and mailbox reads) are read-only and never claim, wake, retry, cancel, or dispatch work. `-retention` (default 7 days) bounds adapter operation receipts: at startup and hourly the service deletes receipts older than it, so retrying an operation after that window runs it again under current lease and delivery state. A claim that found nothing to claim stores no receipt. Retention deletes nothing else yet.

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

## Codex driver

`crew-codex` connects to the shared Codex daemon the TUI uses and delivers
fabric messages into its threads; see the README's Codex driver section for
the delivery rules. It never starts, stops or restarts Codex. Build and
install the binary and `~/.config/systemd/user/crew-codex.service` with:

```sh
cd /home/dev/crew-services
scripts/install-crew-codex.sh --service-only
```

The unit runs:

```ini
ExecStart=%h/.local/bin/crew-codex -fabric-url http://127.0.0.1:8787 -control-listen 127.0.0.1:8788 -state %h/.local/state/crew-codex/threads.json -address crew/scout=YOUR_CODEX_THREAD_ID
```

`-address` pins are optional and repeatable; every loaded thread gets an
`s/...` session address anyway. `-state` remembers which threads have session
addresses. `scripts/install-crew-codex.sh --hooks-only` adds the SessionStart
hook that tells a new thread its address.

## Crew review service backends

The separate `crew-review` command owns the local review job ledger and its
ephemeral runtime pool. It calls the current Den MCP endpoint directly for
`request_review`, GitHub check gate operations, `get_review_context`, and
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
  -den-mcp-url "${DEN_MCP_URL:-http://192.168.1.5:5199/mcp}" \
  -backend "${CREW_REVIEW_BACKEND:-codex}" \
  -review-profile "${CREW_REVIEW_PROFILE:-/home/system/crew-services/reviewer.md}" \
  -codex-model "${CREW_REVIEW_MODEL:-}" \
  -codex-effort "${CREW_REVIEW_REASONING_EFFORT:-}" \
  -capacity "${CREW_REVIEW_CAPACITY:-2}"
```

The command defaults to the existing `codex` backend and also supports `dsh`.
For `dsh`, configure the DSH plugin's loopback control route with
`CREW_REVIEW_DSH_URL` (or `-dsh-url`), for example:

```sh
CREW_REVIEW_BACKEND=dsh
CREW_REVIEW_DSH_URL=http://127.0.0.1:3080/plugins/dsh-crew-messaging/reviewer-runtime
```

The DSH plugin owns its reviewer model, provider, preset, and reviewer-profile
injection. Do not add those settings to `crew-review`; it only sends workspace
and controller-owned review prompts to an opaque DSH worker. The command also
accepts `-den-mcp-token`, `-codex-model`, `-codex-effort`, `-codex-command`,
repeated `-codex-arg`, and `-run-interval`.
`CREW_REVIEW_LISTEN`, `CREW_REVIEW_DB`, `DEN_MCP_URL`, `DEN_MCP_TOKEN`, `CODEX_COMMAND`,
`CREW_REVIEW_MODEL`, `CREW_REVIEW_REASONING_EFFORT`, `CREW_REVIEW_PROFILE`,
`CREW_REVIEW_BACKEND`, `CREW_REVIEW_DSH_URL`, `CREW_REVIEW_CAPACITY`, and
`CREW_REVIEW_RUN_INTERVAL` are the matching
environment settings. Keep the installed deployment values in
`/home/system/crew-services/crew-review.env` and the dedicated instructions in
the adjacent `reviewer.md`.

After changing `DEN_MCP_URL` in that environment file, restart
`crew-review.service` when its review pool is idle. A running process retains
its old environment even when the file already points to the new Den host.
Check the service's configured endpoint and complete a managed submission;
an active systemd unit alone does not establish Den connectivity.

For a persistent agent-box service, save this as
`~/.config/systemd/user/crew-review.service`:

```ini
[Unit]
Description=Crew review runner
Wants=crew-messaging.service
After=crew-messaging.service

[Service]
EnvironmentFile=/home/system/crew-services/crew-review.env
ExecStart=%h/.local/bin/crew-review
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

Codex updates itself on disk under a running crew-review. Before each review
the Codex backend compares the executable's resolved path, modification time,
size and `--version` with the ones its app-server started from, and restarts
the app-server when they differ, once no review turn is running. A review that
fails because the runtime broke (a tool-runtime error such as `failed to
decode code-mode IPC frame` with no `complete_review`, or the app-server lost
mid-turn) restarts the app-server and is retried, up to three attempts per
process. A job that still fails records a `runtime_infrastructure:` failure,
and a plain resubmit of the same submission re-runs it; any other failure
still needs `supersedes_round` or an explicit retry.

When Den MCP runs on a separate service box, keep `crew-review` loopback-only
and carry its backend connection over SSH. On the agent box, a persistent
reverse tunnel such as the following makes the agent-box listener available as
`127.0.0.1:8413` on `den-services` (192.168.1.5) without opening a LAN listener.
On the migrated `den-agents` workstation, use the dedicated SSH configuration
with its existing pinned host key:

```ini
[Unit]
Description=Expose the local Crew review runner to Den MCP over SSH
After=network-online.target crew-review.service
Wants=network-online.target crew-review.service

[Service]
ExecStart=/usr/bin/ssh -F /home/agent/server-access/den-services/config -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R 127.0.0.1:8413:127.0.0.1:8413 target
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
```

Save that unit as `~/.config/systemd/user/crew-review-den-tunnel.service`, then
run `systemctl --user daemon-reload` and
`systemctl --user enable --now crew-review-den-tunnel.service`. Verify the manual
review readiness GET through Den Web as well as the local runner health check.
A healthy local runner with an absent tunnel causes gateway 502 responses for
all manual-review tasks. Restoring the tunnel does not require restarting the
runner or submitting a review. The Den MCP `crew-review` backend remains
`http://127.0.0.1:8413`; from its point of view that address is the remote end
of the tunnel. This topology requires the service account's existing key-only
SSH route to `den-services` and remote forwarding support.

The Den MCP facade's `submit_task_for_review` tool forwards the public
project/task/repository/ref/checks/summary envelope to
`POST /v1/review-submissions` on the loopback `crew-review` backend. Configure
that backend in the MCP route/config files before enabling the tool. The
receipt is immediately `gate_pending` when checks are not terminal; crew-review
advances the submission itself on its submission interval, so callers do not
poll or resend it. A passed gate is admitted exactly once for its Den review
round. Resend the same envelope only after re-running a failed check or after
fixing `changes_requested` findings. If the backend is down, the MCP response is a
retryable `den_backend_unavailable` result; the route never falls back to
Rusty or dispatches a second review authority.

Callers normally omit `commit_sha` and `base_commit`. The review host discovers
the checkout matching `repository`, fetches objects without changing its local
branches or working files, selects the latest task commit on the pushed `ref`
(subject containing `(#<task_id>)`), and derives the parent of the earliest
matching task commit as the diff base. Both exact SHAs are recorded in the
receipt and passed unchanged to the gate and reviewer. Explicit overrides
remain available. Den validates existence and ancestry with GitHub before
recording a round; `unknown_commit` and `base_not_ancestor` refusals are terminal
and do not start gates or background retries. Missing markers and ambiguous
merge paths are reported before admission. If a task spans merge paths, an
explicit containing merge commit can select the complete history.

Optional `supersedes_round` and `supersede_reason` are forwarded to Den's
atomic replacement path. The old round must belong to this task and have no
review findings, finalization, or verdict; Den records the correction reason
in the replacement request and closes the earlier round. The local runner
continues to rely on Den's current round before job admission and finalization.

Run the proportional local checks from the two sibling repositories:

```sh
cd /home/dev/crew-services && go test ./... && go vet ./...
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsc --noEmit -p ../../plugins/crew-messaging/tsconfig.json
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec vitest run --config ../../plugins/crew-messaging/vitest.config.ts
cd /home/dev/dsh-crew && pnpm --dir research/deepseek-harness exec tsx ../../plugins/crew-messaging/scripts/agent-box-probe.ts
```
