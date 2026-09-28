# Prompting adaptive playtest agents

Use this with the [tool guide](playtest.md). The agent chooses short actions from
current observations; the game supplies mechanics and live action timing. A
complete encounter script, Jev, and a fixed RNG seed are unnecessary.

## Parent setup

Before assigning a tester:

- Start a ready product host and register its profile. Supply the profile, service
  URL, repository, mission, ordinary controls, and evidence directory.
- Give each simultaneous gameplay tester a separate native world/host. Browser
  slots isolate browsers, but multiple browsers on one host can share the player,
  time mode, enemies, and health. Reconnecting a browser does not reset that world.
- State whether the tester owns a new browser or an existing session, and who
  owns host reset/cleanup. Preserve unrelated sessions.
- Set an explicit stop condition and budget. Choose enough time for the mission;
  the Doom 20-minute trials ended incomplete, not at death or level clear. Reserve
  time for the interview. Record actual elapsed time if a run exceeds its budget.
- Name the actual tester model and runtime revision separately in the report.
  Tell regression testers what to examine without telling them it is fixed.

## Reusable exploration prompt

Replace the bracketed fields before sending:

```text
Playtest [product/repository] through the installed crew-services playtest CLI
or matching MCP tools. Profile: [profile]. Service: [URL]. Evidence: [directory].
The product host is ready and owned by the parent. [Start your own browser / use
session ID]. Other testers have separate native worlds.

Mission: [e.g. explore until all enemies are defeated or the player dies].
Stop also at [wall-clock deadline / action budget / unrecoverable blocker].
Reserve the last [N minutes] for final evidence and the exit interview. Stop
issuing gameplay actions when the deadline or parent stop instruction arrives.
Ordinary controls: [bindings, or where live discovery provides them].

Begin with profile/reset semantics, capability discovery, an original screenshot,
and a compact gameplay observation. Inspect the screenshot before interpreting
it. Wait for read-only capability discovery if the page is still starting.
Use action-driven time when available. Choose a nearby goal, perform a short
ordinary action, and inspect its actual result before choosing the next action.
Use live action duration unless the experiment specifically needs an override.
Look can change gameplay aim without advancing time. Observer camera movement
only changes the view; restore it before judging ordinary player visibility.

Use semantic facts to find enemies, doors, pickups, and navigation targets.
Check aim, reach, occlusion, cooldown and availability. A submitted action is not
a confirmed hit or interaction. A route is guidance, not proof of arrival.
Avoid long blind action sequences. On uncertain delivery, reobserve before doing
anything else; don't automatically replay an attack or use action.

When movement makes little progress, inspect pose, grounded state and the last
movement receipt. Request probe, clearance or a small grid if available. Use
these to choose a different ordinary action. Do not repeatedly push into the
same obstacle without learning anything. Do not infer that an empty cell or a
support normal guarantees a route. Jump targets use world feet coordinates;
inspect the actual landing. Capture a reproducible blocker before stopping.

Use original screenshots at milestones and failures. Use a held-time survey for
surroundings, or a short recording for fast particles/projectiles. Keep diagnostics
triggered; don't request every large telemetry dump after every action.

Do not edit product code, change profiles, restart services, or build another
browser harness. Report missing capabilities or infrastructure errors. No debug
teleport, forced damage, or direct state mutation unless this mission explicitly
authorizes it; label any such assistance separately from ordinary gameplay.

At the stop condition, save final state and an original screenshot. Stop only
[your browser / specified cleanup policy]; leave the native host to the parent.
Write EXIT_INTERVIEW.md before returning. Include the sections below, even if the
run ends early. Report incomplete progress honestly; don't turn a time limit into
a gameplay pass or failure.
```

## Exit interview

Ask for concrete incidents and evidence rather than a general satisfaction score:

1. **Outcome:** mission reached, death, budget, blocker, or infrastructure error;
   final location, health, objective/enemy progress, elapsed time and action count.
2. **What worked:** ordinary actions and helpers that enabled specific progress.
3. **Difficulties:** where progress stopped, what the agent expected, what happened,
   and which observations/actions it tried. Include the last useful pose/receipt.
4. **Missing or confusing facts:** what information would have changed the next
   decision? Distinguish visual ambiguity, product mechanics and tool limitations.
5. **Suggestions:** rank a few improvements separately for game, tools, and
   telemetry. Suggestions are observations for follow-up, not acceptance gates.
6. **Evidence and cleanup:** original captures, relevant receipts, session/slot,
   final held-time state and browser cleanup result. Mark uncertain diagnoses.

The parent checks suggestions against the receipts. Preserve the interview as
written, but annotate unsupported conclusions in the summary. In our Doom runs,
“traversed the snag area” did not establish reproduction and repair of the exact
old frozen pose. Different routes and diagnostic effort also made kill counts
unsuitable as a direct performance comparison.

## Useful focused missions

- **Door:** approach, inspect refusal/angles, open using ordinary use, cross, then
  confirm state and position. Observer-camera visibility does not prove aim/reach.
- **Movement bump:** save pose and controller contact; inspect a small nearby
  region; attempt a bounded recovery; report actual displacement and landing.
  Floor contact at sweep start can coexist with a clear destination.
- **Fast effect:** trigger an ordinary action while recording; inspect original
  frames, or advance in short windows and inspect while held. No rewind needed.
- **Regression:** reproduce the symptom first if possible. If it never occurs,
  report “not reproduced”; successful nearby traversal is still useful evidence.

Examples and both independent interviews are in the
[Doom clearance report](playtest-evidence/doom-clearance-20260928/README.md).
