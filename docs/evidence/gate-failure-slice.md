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

_Pending._

## Codex and DSH

_Pending._

## Session projection

The fabric's session projection, event log and SSE were deleted in `b26e594`, and migration 9 dropped their tables.

- It held 1,619 events, the last from 2026-09-04.
- Nothing wrote to it during the campaign.
- Its only client was the dsh-crew `crew-messaging` plugin, which #10454 retired.
- The fabric's `GET /v1/sessions` now returns 404. The drivers' own `/v1/sessions` APIs are unaffected.
