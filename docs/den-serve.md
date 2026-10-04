# den-serve Agent Usage

Use `den-serve` when an agent needs to leave a dev or demo app running for human inspection over the LAN. Do not hand-pick a fixed port, bind only to localhost, reuse a random server that happens to answer, or kill an unknown process on a port.

`den-serve` is local-run infrastructure. It lives in crew-services with the other local agent services (it moved from `den-services`); it is not a den-srv service.

## Quick Start

Install it (and its `den-serve-page` status page unit) from `/home/agent/dev/crew-services` with `scripts/install-den-serve.sh`, or run it from source:

```bash
go run ./cmd/den-serve up asha-demo -repo /home/dev/asha-demo
```

The command prints a packet like:

```text
asha-demo running
local: http://127.0.0.1:5173/
lan:   http://192.168.1.22:5173/
state: /home/agent/.cache/den-serve/sessions/asha-demo-abc123/current.json
logs:  /home/agent/.cache/den-serve/sessions/asha-demo-abc123/asha-demo-...
launch source: restarted_stale
started: 2026-07-25T17:00:00Z
launch fingerprint:  0123456789abcdef...
current fingerprint: 0123456789abcdef...
pid:   12345
```

The LAN URL is the one to give the human.

## Commands

```bash
den-serve up <project-id> -repo /path/to/repo
den-serve restart <project-id> -repo /path/to/repo
den-serve status <project-id> [-repo /path/to/repo [-instance name]]
den-serve list
den-serve page
den-serve logs <project-id> [-repo /path/to/repo [-instance name]]
den-serve stop <project-id> [-repo /path/to/repo [-instance name]]
```

`-instance` addresses one of several hosts of a checkout, such as a playtest
session's (its instance is the playtest session ID); it needs `-repo`.

`den-serve page` runs a small status site at `http://<lan-host>:37299/`. It
binds to `0.0.0.0` by default and, on every browser refresh, lists assignments
whose configured identity and health probe currently match. This deliberately
keeps a reachable replacement process visible even when the persisted launch
PID or ownership metadata is stale. Each project name links to its current LAN
port assignment; wildcard or loopback session URLs are rewritten using the
hostname used to open the status page. Keep the command running while using the
page; override `status_page.bind_host` or `status_page.port` in the config file
only when the defaults conflict with another local service.

Broker-owned rows include a **Restart** button. It uses the same owner-aware
stop-and-up flow as `den-serve restart`, then redirects back to the refreshed
listing. Explicitly reused external processes remain visible but cannot be
restarted from the page. Historical records whose manifest no longer exists do
not count as identity-healthy, even if another project later answers on the old
port.

`restart` stops a running broker-owned session and starts it again. If no
session exists (or the previous session is already stopped), it behaves like
`up`. It refuses to stop an explicitly reused external process.

Use `--public-host <ip-or-host>` only when automatic LAN IP detection chooses the wrong address. There is intentionally no prominent `--host` flag: started dev servers bind LAN-facing by default.

`den-serve` has built-in defaults matching `den-serve/config/config.example.yaml`. Use `-config` or `DEN_SERVE_CONFIG_PATH` only when overriding those defaults.

## Repo Manifest

`den-serve` reads the `project` and `serve` block from one repo-root manifest. Lookup order:

- `.den-serve.json`
- `den-serve.json`
- `.den-playwright.json`
- `.playwright-service.json`
- `den-playwright.json`

Minimal manifest:

```json
{
  "project": "asha-demo",
  "serve": {
    "command": "npm run dev -- --host {host} --port {port}",
    "preferredPort": 5173,
    "healthUrl": "/health",
    "readyText": "\"project\": \"asha-demo\"",
    "identityHeader": "X-Den-Project",
    "reusePolicy": "broker_owned",
    "fingerprintPaths": ["dist/native/asha_engine.node"],
    "startupTimeout": "45s"
  }
}
```

`identityHeader` must return the project ID by default. Set
`identityHeaderValue` when a host contract uses a different stable value, such
as `"browser-host.v0"` for `X-ASHA-Browser-Host`; pair that host identity with
`readyText` when it must independently identify the project.

`den-serve` does not require a Playwright `tests` block when reading `.den-playwright.json`.

The launch fingerprint always includes the resolved `serve` definition. In a
Git checkout it also includes `HEAD`, tracked dirty content, and untracked
non-ignored files. Use `fingerprintPaths` for ignored or generated inputs whose
bytes affect the loaded process, especially native addons. Entries are relative
files or directories below the repo root. Keep directory entries narrow so
fingerprinting does not walk an entire build cache.

Template variables available in serve commands and manifest env values:

- `{project}`
- `{repo_root}`
- `{host}` / `{bind_host}`
- `{probe_host}`
- `{port}`
- `{local_url}`
- `{public_url}`
- `{session_dir}`
- `{instance}` and `{label}`, for an instance session (see Instances)

### Instance, keep and stop commands

Three optional `serve` fields let a host take part in owner-aware lifecycle.
Each is appended to, or run beside, the ordinary `command`, so the command must
end with the host invocation itself (`exec rusty dev …`, or a script that
forwards its arguments):

- `instanceArgs` is appended for an instance session. A host that allows one
  session per project needs it to run several. For an Engine product on a pair
  from rusty-engine `0807adf51` or later:
  `"instanceArgs": "--instance {instance} --label {label}"`. `{label}` is the
  owner's label, `crew-playtest:<session>` for a playtest session, so
  `rusty dev list` shows which session owns each host.
- `keepArgs` is appended when the owner asks the host to stay up unused, as a
  kept playtest session does: `"keepArgs": "--keep"`.
- `stopCommand` runs from the repo root before den-serve signals the host's
  process group, so the host can stop its own process tree. Rendered at launch
  and kept with the session, so later manifest edits cannot redirect it:
  `"stopCommand": "PATH=\"$HOME/.local/bin:$PATH\" rusty dev stop --project ./src/Game/Game.csproj --instance {instance}"`.
  A failed stop command is reported, and the process group is signalled anyway.
  An ordinary (non-instance) session renders `{instance}` empty, so give it a
  stop command only if that still addresses it.

Add these only for a host that accepts them. An older Engine pair refuses
`--instance` and would fail to start, which is why den-serve never adds them by
itself. `rusty dev stop` and `rusty dev list` run in whichever `rusty` is
invoked, so the one on PATH must be from `0807adf51` or later.

## Safety Rules

If `preferredPort` is occupied:

- matching health + matching broker-owned lease: reuse is allowed;
- matching health + `"reusePolicy": "explicit"`: reuse is allowed, but `den-serve stop` will not kill it;
- wrong health, wrong text, wrong identity header, or unowned broker-owned policy: `den-serve` chooses a fallback port;
- no safe port: the command fails clearly.

Broker-owned dev servers are started in their own process group. `stop` only stops broker-owned process groups with recorded live sessions. It never kills arbitrary user processes.

When `up` finds a healthy broker-owned session whose launch fingerprint differs
from the intended checkout, it stops that process group and starts a fresh
session. A stale explicitly reused unowned process is never killed; `up` returns
`den-serve session is stale` and tells the caller to restart the external host.

Native addons are loaded into process memory. Replacing a `.node` file on disk
does not update a Node process that already loaded it. A matching health response
therefore proves app identity and readiness, not source freshness; the launch
fingerprint is the freshness proof.

## Session State

Session state lives under `~/.cache/den-serve/sessions` by default and is keyed by project plus repo path. Two worktrees with the same project id get separate session records. `status`, `list`, `logs`, and `stop` work from this persisted state.

`list` and `up` prune that state: leases whose process group has gone, the
directory and logs of a broker-owned session whose processes ended more than
`retention` ago (72h by default; set `retention: 168h` in the config file to
keep a week), and earlier launch directories beside a current session after the
same time. Records of hosts den-serve does not own are left alone.

Fallback ports come from the managed range, 30300-30450 by default. It sits
below Linux's ephemeral port range (32768-60999): a port inside that range can be
taken as the local end of an outgoing connection while a product builds, and the
product's own bind then fails. A manifest's `preferredPort` is still tried first
for the ordinary session.

## Instances

A tool that needs several separately owned hosts of one checkout starts named
instances (`UpOptions.Instance`). The crew playtest service starts one per
session for hosted profiles, so each tester gets its own world. Instance session
state is keyed by project, repo path and instance name; instances never adopt the
preferred port or a healthy host that belongs to another session. `den-serve
list` and the status page show them as `project · instance (label)`; the page
offers no Restart for them, because their owner stops and replaces them. Stop
one by hand with `den-serve stop <project> -repo <repo> -instance <name>`.

`status` recalculates the current fingerprint without changing the launch
fingerprint. It reports `stale` plus a reason when repo `HEAD`, dirty/explicit
inputs, or the resolved launch definition drifted after the process started.
