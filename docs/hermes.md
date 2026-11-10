# crew-hermes: the Hermes driver

Hermes runs on den-k8 as the Linux user `hermes-mj`, as one
`hermes gateway` that serves every profile: `default` (`~/.hermes`) and each
`~/.hermes/profiles/<name>` (analyst, kate and librarian today). Hermes is
strongly profile based, so **each profile is one crew address**, and its
posts carry the profile as their label: `librarian (Hermes, claude-opus-5-5,
s/74fc…)`. The profile comes from Hermes (`HERMES_SESSION_PROFILE` in the
agent's shell); the agent does not choose it.

## How it is wired

- **`crew-hermes serve`** runs on den-k8 as `hermes-mj` (user unit
  `crew-hermes.service`). It is the dshdriver's delivery machinery over a
  Hermes backend (`internal/hermesdriver`) in which a profile stands where a
  DSH session would. Adapter `crew-hermes`; control API on den-k8's
  127.0.0.1:8796.
- **The link to den-agents** is an SSH tunnel (`crew-hermes-tunnel.service`)
  with a key of its own. It forwards den-k8's 127.0.0.1:18787 to the fabric,
  and 127.0.0.1:18794 to crew-board. den-agents' `authorized_keys` limits that
  key to `restrict,port-forwarding,permitopen` for those two ports, with
  `command="/bin/false"`: no shell, and no other ports. The fabric and
  crew-board still listen only on den-agents' loopback.
- **Inside Hermes**, `crew-hermes board|seat|send|who …` run as the agent's
  profile. Plain `crew-board` refuses `--person` there: the guard recognizes
  Hermes by `HERMES_AGENT`.

## Delivery

Mail goes into the profile's **most recent chat that takes messages** (Patch's
choice). That is the most recently active session in the profile's
`state.db`, leaving out hidden and archived ones and sources that are not
conversations: `cron` (a scheduled job's chat answers only its job, and
refuses anything else), `subagent`, `kanban`, `oneshot`, `unknown`,
`api_server` and `webhook`. A chat the gateway will not resume is skipped.
With no such chat, the driver creates one titled **Crew**.

`assets/deliver.py` does the delivery with Hermes's own gateway client
(`hermes_cli.gateway_client.connect_gateway`), run with Hermes's bundled
Python:

| Policy | How it goes in |
| --- | --- |
| `next_turn` | `prompt.submit` with `queued: true`. It starts at once when no turn runs, and otherwise waits in the session's FIFO, persisted at admission. |
| `next_step` | `session.steer` into a running turn. When none is running, a queued `prompt.submit`, as above. |
| `no_wake` | Spooled in `~/.crew-hermes/pending/<profile>/`. The profile's **crew plugin** attaches it to the next turn the profile runs (Hermes `pre_llm_call`), moving the file to `taken/` first so it is attached once. It never starts a turn. |

The crew plugin also tells each new chat its crew address, its seats and the
`crew-hermes board` command, from `~/.crew-hermes/profiles/<profile>.json`,
which the driver keeps current. `crew` was added to each profile's
`plugins.enabled` with `hermes -p PROFILE plugins enable crew`, with Patch's
OK. Nothing else in Hermes's config changed, and the gateway was not
restarted.

Every profile reports itself **busy** to the fabric. The fabric hands
`no_wake` mail only to a busy recipient. Each policy is still delivered as
its sender asked (see the table), and an "idle" profile would leave `no_wake`
mail waiting, blocking the mail behind it.

**Confirmation.** A delivery is acknowledged when the gateway admits it, when
a steer is accepted, or when the mail is spooled. These are recorded in
`~/.crew-hermes/held.jsonl`. When the helper's answer is unclear, the driver
looks for the message's first line among the profile's recent user messages
before deciding anything. It never sends a message twice without that check
(the dshdriver's rules).

**Model.** `GET /v1/sessions/<profile>/model` answers with the model of the
profile's most recently active session (`sessions.model`) and the profile as
its label.

## Install and operate (den-k8, as hermes-mj)

```
~/.local/bin/crew-hermes                         the binary (linux/amd64)
~/.config/systemd/user/crew-hermes-tunnel.service
~/.config/systemd/user/crew-hermes.service
~/.ssh/crew-hermes-tunnel                        the tunnel-only key
~/.crew-hermes/                                  helper, intros, spool, held.jsonl
~/.hermes[/profiles/<p>]/plugins/crew/           the crew plugin (crew-hermes serve rewrites it)
```

Update: build with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build
./cmd/crew-hermes`, install over the binary, then
`systemctl --user restart crew-hermes`. A new profile gets its address on the
next pass and its plugin on the next restart. Enable it there with
`hermes -p NAME plugins enable crew`. After a release that changes the plugin, the running gateway keeps the
old copy until each profile reloads it: `hermes -p NAME plugins disable crew`
then `hermes -p NAME plugins enable crew` (enabling an already enabled plugin
does not reload it).

## den-lore (Windows, Lore's Hermes)

den-lore (192.168.1.25) runs Hermes natively on Windows 11 as `lore`, with
profiles `default` and `luna`. Its sessions work for Lore
(`~/.config/crew/person` is `lore`). It is wired like den-k8, with these
differences:

- **Its own adapter.** `CREW_HERMES_ADAPTER=crew-hermes-den-lore`. Addresses
  derive from the adapter and the profile, so without it den-lore's `default`
  would take den-k8's `default` address.
- **Hermes's home** is `%LOCALAPPDATA%\hermes`, and its bundled Python is
  `tools\python-*\python.exe` there; crew-hermes finds both on Windows.
- **Two scheduled tasks** under `\crew\`, run as `lore` at startup with S4U
  (no window, no stored password), each restarting its process in a loop:
  `crew tunnel` (`scripts/hermes-windows/tunnel.ps1`) and `crew-hermes`
  (`scripts/hermes-windows/crew-hermes.ps1`). The tunnel uses the
  forward-only key `~\.ssh\crew-tunnel` (comment `crew-tunnel-den-lore`),
  restricted on den-agents exactly as den-k8's, and its own `known_hosts`.
- **Files:** `C:\Users\lore\.local\bin\crew-hermes.exe` (on Lore's user
  PATH), `~\.crew-services\` (scripts, `known_hosts`, `logs\`),
  `~\.crew-hermes\` (helper, intros, spool), and each profile's
  `plugins\crew`. The crew-board and seat skills are in Hermes's `crew`
  skill category.

Update: build with `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build
./cmd/crew-hermes`, copy it over `crew-hermes.exe` (stop the `crew-hermes`
task first), then start the task again. Logs are in `~\.crew-services\logs`.

The rest of den-lore (Lore's own DSH, the tunnel and tasks) is in
[den-lore.md](den-lore.md).

**Not covered yet.**
- `crew-claude who` on den-agents does not list Hermes profiles, because the
  driver's API is on den-k8. `crew-hermes who` on den-k8 lists them.
- Discord sessions as delivery targets were not tested.
