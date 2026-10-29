# Gate-failure slice: end-to-end evidence (#10456)

Recorded 2026-10-29 on den-agents, against the deployed services:

- `crew-messaging` (the fabric)
- `crew-review`
- `crew-claude`, `crew-codex`, `crew-dsh`
- den-services `mcp` at `6894980`

## Setup

- **Scratch target.** The branch `scratch/gate-proof` of FuzzySlipper/crew-services carries a `gate-proof` workflow. It fails on purpose until `gate-proof/PASS` exists.
- **Scratch tasks.** Submissions were made for scratch Den tasks: #10472 for the Claude cases, plus separate scratch tasks for Codex, DSH and the exited case. All are children of #10456.
- **Claude submissions.** These went through the ordinary MCP `submit_task_for_review` from the coordinator Claude Code session (`s/6d10f4999fd5a972`), with no `notify_address` argument. The `crew-claude hook fill-notify` PreToolUse hook filled it in. crew-review stored `notify_address = s/6d10f4999fd5a972` for submission `63a9015d…`.
- **Where the evidence comes from.**
  - Notice rows are from crew-review's `crew_review_notices`.
  - Message and delivery rows are from the fabric's `messages` and `deliveries`.
  - Times are UTC.

One false start: the first scratch commit had a YAML error in the workflow (`: ` inside a plain scalar), so GitHub never ran a job. Den correctly left that gate (8396) pending. Round 7056 was superseded once the file was fixed.

## Claude Code

### Busy: seen at the next tool boundary, turn not cancelled

Submission `a2c4927d…`, gate 8397, commit `5466850`.

1. The coordinator stayed in one long turn of tool calls (sleeps and polls) while the check ran.
2. The check failed at about 10:45:30, and crew-review recorded `gate_failed` by 10:45:56.
3. The notice operation was `submission/a2c4927d…/gate_failed/8397`. It went from `pending` to `sent` after 1 attempt, as message `76a1f86d…`, created 10:46:05.
4. The fabric delivery:
   - policy `next_step`, dispatch action `deliver_next_step`;
   - claimed 10:46:07, `delivered` / `native_accepted` at 10:46:43;
   - attempt_count 1.
5. The notice appeared in the coordinator's transcript between two tool calls of that same turn, and the turn carried on.

The text it carried:

```
crew-review: the required checks for task #10472 "Scratch: gate-failure notice proof target (#10456)" (crew-services) failed, so no reviewer was started.
Commit 546685082e7b31ee30aa09a0ab5efb9907824b24 on scratch/gate-proof in FuzzySlipper/crew-services.
Checks that did not pass:
- gate-proof (failure): https://github.com/FuzzySlipper/crew-services/actions/runs/40859965145/job/123400407590
Next: fix the cause, or re-run the failed check if it was flaky. Then call submit_task_for_review again with the same arguments (with the new commit_sha if you pushed a fix).
```

As intended for a `system/` origin, there was no reply line.

### Restarts: fabric down when the gate fails, then both services restarted

Submission `b1e43925…`, gate 8398, commit `bee8a14`.

| Time | Event |
| --- | --- |
| 10:47:08 | `crew-messaging` stopped |
| 10:47:45 | `gate_failed`; notice row `pending`, attempts 0 |
| 10:48:09 | `pending`, attempts 1, last_error `dial tcp 127.0.0.1:8787: connect: connection refused` |
| 10:48:20 | `crew-review` restarted |
| 10:48:23 | `crew-messaging` started |
| 10:48:45 | fabric accepted message `32bbb62c…` |
| 10:48:52 | notice row `sent`, attempts 2 |
| 10:49:41 | delivery `delivered` / `native_accepted` (`deliver_next_step`, attempt_count 1), mid-turn |

Exactly one fabric message and one delivery exist for that operation. After the busy and restart cases, crew-review had sent the fabric two messages in total, one per failed gate.

### Idle: the notice starts a new turn

Submission `b08eae89…`, gate 8399, commit `7eea11b`.

1. The coordinator ended its turn right after submitting.
2. Message `92830eb2…` was created at 10:51:15.
3. It was claimed as `deliver_at_idle` at 10:51:17 and settled `delivered` / `native_accepted` at 10:51:19, with attempt_count 1.
4. The notice started a new coordinator turn without any user input.

### Re-run: passing check, no notice, review admitted

Submission `8e76cf0a…`, gate 8400, commit `70663e9` (adds `gate-proof/PASS`).

- The phase went from `gate_pending` to `job_admitted` by 10:52:27.
- No `crew_review_notices` row was written for the submission.
- The review pool showed a reviewer job running for #10472.

### Exited: delivered on resume

Scratch task #10483, submission `7ae3e633…`, gate 8406, commit `72af331`.

**Test subject.** A throwaway desktop Claude Code session registered as `s/18b3ce4f7fa49522`.

**Making it exit.** The session was archived, which stops its process. crew-claude then listed it as `exited`, and no process remained.

**Submission.** It went through `den-tool den submit_task_for_review` with `notify_address: "s/18b3ce4f7fa49522"`. This was the real MCP path; the coordinator's own tool schema predates #10455.

| Time | Event |
| --- | --- |
| 11:27:50 | `gate_failed` |
| 11:28:15 | fabric accepted message `c8fc385e…` |
| 11:28:20 | notice row `sent` |
| 11:28:20–11:28:57 | delivery `queued`, attempt_count 0: nothing is attempted while the session is exited (Claude advertises no `wake_inactive`) |
| 11:29:11 | session unarchived and resumed with a neutral message; SessionStart re-registered the same address. Same instant: claimed as `deliver_at_idle` |
| 11:29:13 | the session answered the resume message |
| 11:29:15 | the notice arrived as its own peer turn (`turnOrigin: peer`) |
| 11:29:17 | `delivered` / `native_accepted`, attempt_count 1; the session replied `got notice c8fc385e…` |

Exactly one message and one delivery exist for that operation.

## Codex and DSH

Live run on 2026-10-29, 10:53Z to 11:05Z, against the deployed services: crew-review on 127.0.0.1:8413, crew-messaging on 127.0.0.1:8787, crew-dsh and crew-codex. No service was restarted.

**Setup**

- Scratch tasks: #10473 for DSH and #10474 for Codex. Both had parent #10456, priority 5 and the tag `scratch`. Both were set to in_progress before submitting and cancelled afterwards.
- Every failing case got its own commit on `scratch/gate-proof` with `gate-proof/PASS` removed.
- Each submission went through `den-tool den submit_task_for_review` with `required_checks:["gate-proof"]` and an explicit `notify_address`. `crew_review_submissions.notify_address` was checked right after each submit, and it held the address in every case.
- Targets:
  - DSH scratch session A: `session-37444973-2dcb-48fe-96b5-efe43abbafcc`, address `s/711a4c1647dd319f`.
  - Codex scratch thread: `01a1869e-bc1f-7a43-90ca-b9b16da2c8cb`, address `s/2788fa8670699756`. Its rollout is `~/.codex/sessions/2026/10/29/rollout-2026-10-29T00-45-12-01a1869e-bc1f-7a43-90ca-b9b16da2c8cb.jsonl`.
- Raw logs (kept locally, not committed, because they are session transcripts) are at `/home/agent/session-rescue/10456-raw/` on den-agents: `case{1,2,3,4}.log` (3 s polls of the submission phase, the notice row and the session state), `case1-dsh-events.txt`, `case2-dsh-events.txt`, `case3-codex-rollout.txt` and `case4-codex-rollout.txt`.

Every notice had the same shape: producer `crew-review`, sender `system/crew-review`, `activation_policy` `next_step`, and a 24 h expiry. Each one was delivered on the first attempt (`attempt_count` 1), with terminal reason `native_accepted` and native reference `<driver>:<delivery_id>`.

### 1. DSH idle: PASS

- Commit `465377a` (#10473). Submission `4ce3fabd-aa94-4d09-ab78-f5ec696720bc`, gate 8401, submitted at 10:53:47Z.
- **State when the gate failed** (`gate_failed` at 10:54:45.69Z): `session/state` returned `active:true, running:false, queue:[]`, so the session was loaded and idle. It stayed that way until the delivery.
- **Notice row:** `submission/4ce3fabd-aa94-4d09-ab78-f5ec696720bc/gate_failed/8401`, state `sent`, attempts 1, message `14bd22c24c7f1f6a87f1eb4730f537a1b8074c9239986d97`. Created 10:54:45.668Z, sent 10:55:15.670Z.
- **Fabric rows:**
  - Message created 10:55:15.659Z.
  - Delivery `6bba346fa9e91705fd8bf94d8ee6139240eb4e77782bbdfe`: `delivered`, `dispatch_action` `deliver_at_idle`, `terminal_reason` `native_accepted`.
  - Claimed 10:55:16.677Z, dispatching 10:55:16.694Z, terminal 10:55:16.728Z.
- **Native evidence** (`session/events`):
  - seq 118, 10:55:16.708: `agent/inbox/spliced` with target `next-step`, inserting the notice text.
  - seq 119, 10:55:16.709: `turn/start` turn 7.
  - seq 122: `user/message` with `source.rpcId` `crew-delivery:6bba346f…`, carrying "Crew message from system/crew-review … required checks for task #10473 … failed".
  - The session worked on it for 12 steps, only reading the crew-review and fabric DB rows and the repo, and finished with `turn/end` `completed`. Its reply summarised the notice. It changed no files and pushed nothing.
- About 1 s passed from the fabric accepting the notice to the turn starting. Most of the end-to-end time was crew-review's own pace: about 60 s for the gate, then about 30 s from `gate_failed` until the notice was sent.

### 2. DSH busy (steer at a step boundary, no cancel): PASS

- Commit `973e167` (#10473). Submission `b60259ff-7623-4775-880a-c5ece003c300`, gate 8402, submitted at 10:57:05Z.
- **Busy turn:** a `crew-claude send` at 10:56:51Z started turn 8 at 10:56:52.705. It called `bash` `sleep 180; echo done` (job registered 10:56:56.919).
- **State when the gate failed** (10:58:15.73Z): `active:true, running:true, queue:[]`, with the sleep about 79 s in.
- **Notice row:** `…/gate_failed/8402`, state `sent`, attempts 1, message `73fd49484a9468b6afe867a7975986570cbba508e7da1f30`. Created 10:58:15.702Z, sent 10:58:45.705Z.
- **Fabric rows:**
  - Message created 10:58:45.691Z.
  - Delivery `375df4e6fac57a78b156f703d47688af217ef14bb8ce2e6e`: `delivered`, `dispatch_action` `deliver_next_step`, `native_accepted`.
  - Claimed 10:58:46.679Z, dispatching 10:58:46.701Z, terminal 10:58:46.717Z.
- **Native evidence:**
  - seq 257, 10:58:46.714: `agent/inbox/spliced` with target `next-step`, inserting the notice. `session/state` then showed `queue` length 1. This was 110 s into the sleep.
  - seq 258, 10:59:56.992: `job/settled` for the sleep. It ran its full 180 s; the job started at 10:56:56.919.
  - seq 259: `tool/result` `done`.
  - seq 260: `step/end` step 1.
  - seq 261, 10:59:56.995: the inbox spliced, `removedCount:1`.
  - seq 262: `step/start` step 2.
  - seq 263: `user/message` with `rpcId` `crew-delivery:375df4e6…`, carrying the notice. This is step 2 of the same turn 8.
  - seq 275: `turn/end` turn 8 with reason `completed`.
- The model said "the 180s sleep completed in full and the turn was not cancelled … steered in at the step boundary".

### 3. Codex not loaded (background wake): PASS

- **Thread state:** it was **notLoaded** throughout. `crew-dsh who` showed `not loaded`, last seen 3 h earlier.
- Commit `3952aff` (#10474). Submission `231f371f-7737-4ea8-9d00-ef9a13f8586e`, gate 8403, submitted at 10:59:14Z.
- **State when the gate failed** (11:00:15.69Z): not loaded.
- **Notice row:** `…/gate_failed/8403`, state `sent`, attempts 1, message `86922dabf429c225fbea049b47cc5396dc8caade5ece9300`. Created 11:00:15.665Z, sent 11:00:45.705Z.
- **Fabric rows:**
  - Message created 11:00:45.678Z.
  - Delivery `f307b8e27e3c664a9b2c4b76a1f19baf34cae7a49608babb`: `delivered`, `dispatch_action` `wake_then_deliver`, `native_accepted`.
  - Claimed 11:01:12.256Z, dispatching 11:01:12.287Z, terminal 11:01:12.436Z.
  - The claim came about 27 s after acceptance, which is the next not-loaded pass.
- **Native evidence** (rollout lines 32–44):
  - 11:01:12.396Z: `thread_settings_applied`, the resume.
  - 11:01:12.440Z: `task_started` turn `01a18752-2e72-7282-ae19-d2cda21d5f60`.
  - The developer message gives the crew address.
  - The user message "Crew message from system/crew-review … task #10474 … failed", with `client_id` `crew-delivery:f307b8e2…`.
  - The assistant replied "Received the notification: #10474's `gate-proof` check failed, so no reviewer was started." Then `task_complete`.
- Afterwards the thread showed as `idle`, which means loaded.

### 4. Codex busy (steer, no cancel): PASS

- **Busy turn:** a `crew-claude send` at 11:01:49Z started turn `01a18752-c24a-70f1-9dbe-c54279967281` at 11:01:50.287Z. At 11:01:53Z it ran `exec_command` `sleep 180` (`yield_time_ms` 1000), then polled the process with `write_stdin`, waiting up to 50 s each time.
- Commit `12f55c4` (#10474). Submission `8cba88cf-1f96-49e0-b0f6-cecee69285d9`, gate 8405, submitted at 11:02:02Z.
- **State when the gate failed** (11:02:45.7Z): busy, with the turn active and the sleep about 52 s in.
- **Notice row:** `…/gate_failed/8405`, state `sent`, attempts 1, message `4a4ce0d329f1313e9c389cd6c714d73ba9af59a94d98d166`. Created 11:02:45.689Z, sent 11:03:15.663Z.
- **Fabric rows:**
  - Message created 11:03:15.651Z.
  - Delivery `a58518c51eb90dca534110982cf4a228d2f66af9328ce984`: `delivered`, `dispatch_action` `deliver_next_step` (a `turn/steer`), `native_accepted`.
  - Claimed 11:03:16.246Z, dispatching 11:03:16.266Z, terminal 11:03:16.280Z.
- **Native evidence** (rollout lines 45–114):
  - 11:03:16.280Z: the in-flight `write_stdin` poll returned early, at 14.7 s of its 50 s wait, with "Script running with cell ID 3". The sleep kept running.
  - 11:03:16.287Z: a user message with the notice, `client_id` `crew-delivery:a58518c5…`, recorded in the **same turn**.
  - The assistant said "Received the new #10474 gate-failure notice for `12f55c4`. The sleep test is still running." It then went back to polling.
  - 11:04:53Z: `CommandExecution` `sleep 180` completed with `exit_code` 0 and a duration of 179.9 s.
  - 11:05:24Z: `task_complete` for the same turn. The rollout has no `turn_aborted`.

### Oddities

- **Codex steer and tool wait (case 4).** The steer ended the model's in-progress `write_stdin` wait early, at 14.7 s of 50 s, so the model could read the steered message. The command itself was not interrupted, and the turn was not cancelled. This is the Codex step boundary for a long-running exec, so it does not break the no-cancel rule. It is still worth knowing that a steer shortens a tool's poll wait.
- **Codex sandbox (case 4).** At the end of the turn, the Codex thread tried to reply with `crew-codex send` and reported that sandbox networking blocked it. This does not affect the notice path.
- **DSH acts on the notice (case 1).** The DSH session took the idle notice as work and spent a 12-step turn reading the scratchpad, the gate-proof worktree and both databases before it summarised. It changed nothing. A real session could act on such a notice, for example by pushing a fix, which is the intended effect.
- **Gate 8404** went to some other submission between cases 3 and 4; nothing of mine used it.
- **Task status after a failed gate.** After each gate failure, Den left the task in `review` with no reviewer admitted. Both scratch tasks were cancelled from that state.

**Cleanup**

- `gate-proof/PASS` was restored with commit `f2e39d2` ("Scratch: restore gate-proof/PASS (#10474)") and pushed to `scratch/gate-proof`. Nothing was committed to crew-services main.
- Both scratch sessions were left idle.

## Session projection

The fabric's session projection, event log and SSE were deleted in `b26e594`, and migration 9 dropped their tables.

- It held 1,619 events, the last from 2026-09-04.
- Nothing wrote to it during the campaign.
- Its only client was the dsh-crew `crew-messaging` plugin, which #10454 retired.
- The fabric's `GET /v1/sessions` now returns 404. The drivers' own `/v1/sessions` APIs are unaffected.
