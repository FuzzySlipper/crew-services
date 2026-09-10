# Unprompted tool-choice trial, 2026-09-08

The evaluator was asked to assess Space's low/high thrust and steering,
combined turning/thrust, and release/reset behavior, using native inputs and
visible evidence. It received a prepared session, the `playtest` CLI, and the
ordinary runbook. It was not instructed to use scripts.

In this one trial, the evaluator noticed the JS API but chose direct `input`
and `observe` calls. Its retrospective explanation was that it wanted to inspect
each short probe before choosing the next, and script setup/polling added
overhead for that sequence. This is one task/agent observation, not a general
claim about agent preference or the usefulness of scripting.

The [gameplay report](open-ended/rusty-space-open-ended-2026-09-08.md) retains
the screenshots and conclusions. The trial also exposed actionable harness
friction: invalid keyboard input degraded the session, and an unknown
`back:true` field was silently ignored. The latter invalidated the reset probe;
it was not evidence of a game bug. The implementation now validates before
dispatch and rejects unknown native input fields.

Separate implementation acceptance uses actual JS controller/keyboard programs,
checkpoint screenshots, and interruption of a held virtual controller. Those
guided tests establish functionality, not spontaneous scripting adoption.
