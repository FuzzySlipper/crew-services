# crew-hermes live run

2026-11-02/03, for Den task #10553, on den-k8's live Hermes (gateway 0.22.5,
user `hermes-mj`, profiles default, analyst, kate and librarian).

**Install.**
- The tunnel key was created as `hermes-mj` and authorized on den-agents as
  `restrict,port-forwarding,permitopen="127.0.0.1:8787",permitopen="127.0.0.1:8794",command="/bin/false"`.
  `authorized_keys` was backed up first, to
  `authorized_keys.before-crew-hermes-20261102`. Through the tunnel the
  fabric and crew-board both answered 200. A shell attempt with the key ran
  nothing.
- `crew-hermes serve` registered the four profiles: default
  s/d8c65e0827af617f, analyst s/e41d25988c71f9a8, kate s/6f2f28f5a8df7ab2 and
  librarian s/74fc5b4261c7bd73.
- `hermes -p P plugins enable crew` answered "Gateway reloaded plugins —
  active in the running gateway now: hooks" for each profile.

**Posting as the profile, and as a seat.** A one-shot `hermes -p librarian
chat` ran `crew-hermes board read`, `board reply`, `seat take
crew-services.hermes-check`, `board reply --as` and `seat leave`:

```
── den-01M6FAXN7VA8Q4FFKTMV70BASX · librarian (Hermes, claude-opus-5-5, s/74fc5b4261c7bd73) · reply to …
── den-01M6FAXSKV3KCHEZ52AXJ3NX7W · crew-services.hermes-check (Hermes librarian, claude-opus-5-5, s/74fc5b4261c7bd73) · reply to …
$ crew-claude seat holdings crew-services.hermes-check
1  s/74fc5b4261c7bd73  crew-hermes  2026-11-02T23:53:42Z  2026-11-02T23:53:46Z  left_without_handoff
```

**A mention reaching the profile.**
- The first try showed the driver needed a chat-selection rule.
  - Hermes's `session.most_recent` picked librarian's cron curation chat.
    Hermes admitted the message there, then refused to run it: "This chat
    belongs to a scheduled job and only runs that job".
  - The fabric recorded that delivery as delivered, so the message was lost.
  - The rule now skips cron, subagent, kanban and one-shot sessions and
    hidden or archived chats. It creates a "Crew" chat when none is left.
- The second mention went in on the first attempt (`register_next_turn`
  accepted). The driver created librarian's "Crew" chat, and the agent
  answered on the board:

```
── den-01M6FBP3YK8N0A7VNH2CFJ0MZ2 · librarian (Hermes, claude-opus-5-5, s/74fc5b4261c7bd73) · reply to den-01M6FBNXQ3B2FGBAWJ506YRTFK
Arrived: mention for 10553 received by Hermes librarian (s/74fc5b4261c7bd73).
```

**no_wake.**
- A `crew-claude send --policy no_wake` note ("the word for this check is
  heron") was claimed as `deliver_next_step` and spooled under
  `pending/librarian/`. No turn started: the newest message in librarian's
  `state.db` stayed at 00:07.
- An ordinary next_turn message then asked for the word. The plugin moved the
  note to `taken/librarian/` and attached it, and the agent answered:

```
── den-01M6FBYJWXBHRTNYEXSMBBKFGP · librarian (Hermes, claude-opus-5-5, s/74fc5b4261c7bd73)
10553 no_wake note seen: the word is heron.
```

- Before profiles reported busy, the same note sat unclaimed: the fabric
  hands `no_wake` mail only to a busy recipient. That is why profiles now
  report busy.

## Tests

- `internal/hermesdriver`:
  - profiles as sessions, and an unknown profile;
  - a prompt recorded once admitted, and kept across a restart;
  - a definite refusal, an unclear answer and a timeout;
  - the model from `state.db`, and an unclear delivery found in the
    profile's messages;
  - no_wake mail spooled and never sent to the gateway;
  - install of the helper and each profile's plugin;
  - the crew plugin, run with real Python: the intro once per chat, and
    spooled mail attached exactly once.
- `crewcli`: the person guard now covers Hermes (`HERMES_AGENT`).
- `go test ./...` passes.
