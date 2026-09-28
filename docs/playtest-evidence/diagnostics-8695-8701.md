# Tasks 8695 and 8701 verification

Verified 2026-09-27. No commits created.

## Behavior

- Unknown profile diagnostics distinguish an unloaded on-disk profile, an absent
  profile, and an unreadable or invalid profile file. Inspection does not reload.
- CLI usage explains that `reload` re-reads game profiles and pool configuration.
- HTTP input validates before calling the pool and returns HTTP 400 with code
  `invalid_input`. Direct session validation also preserves the named error.
- Pool result decoration skips nil maps, preserving errors from all operations.

## Reproduction and proof

Evidence directory: `/home/agent/.local/state/crew-playtest-local/verification-8695-8701`.
`verify.py`, `verification.json`, and both service logs preserve the live checks.

The previously installed binary reproduced a closed HTTP connection for
`[{"kind":"hold","keys":["W"],"ms":1}]`, with:

```
http: panic serving ...: assignment to entry in nil map
/home/agent/dev/crew-services/internal/playtest/pool/pool.go:202
```

The rebuilt binary returns `invalid_input: key must be an integer in [1, 255]`.
Malformed hold, move, point, click, wait, gamepad and unknown-kind batches were
rejected. Each was followed by successful valid input and connected status.
A DOM click then produced `Input received`; the original capture is recorded
in `verification.json`. Both test sessions stopped and released their slots.

Adding profile B to the isolated games file produced the explicit reload hint.
After reload, B started successfully while A remained usable.

Passed `go test ./...` and focused race tests for `cmd/playtest-service`,
`cmd/playtest`, `internal/playtest/pool`, and `internal/playtest/session`.
Regression tests additionally cover missing kind, empty batch, direct pool
calls, absent profiles, malformed profile files, and read-only diagnostics.

## Deployment

Deployed 2026-09-28 01:34 UTC after confirming zero active sessions. An old
manually launched process held port 48200; it was stopped while idle and the
service restored under `crew-playtest.service` systemd ownership.

The running executable SHA-256 matches the tested binary:
`3feebba2b845e25fcebb9cb02df53986b7cf651680f15d3cc566d3f1f4aba66b`.
`deployment.json` in the evidence directory records a live malformed-input error,
successful subsequent valid input and browser click, reload with six profiles
and capacity 10, and zero occupied slots after cleanup. Original binaries are
retained in `pre-deploy-backup`. Crawler live-check notes now describe the returned
validation error. No other tester's session was ended.
