# Adding adaptive playtesting to an Engine product

Start with [tester prompts](playtest-agent-prompts.md) and the
[assist tool contract](playtest.md#adaptive-engine-playtesting). Doom is a working
reference for a thin C# adapter. Its game-specific target names, observations and
bindings are examples; each game defines its own useful facts and actions.

## Ownership

| Layer | Responsibility |
| --- | --- |
| Engine | Native simulation clock, forward advancement, input path, renderer/observer, collision queries, shared debug modules |
| Product C# | Live gameplay facts, action bindings/timing/eligibility, look rules, target meaning, current controller and collider inputs |
| Crew playtest | Sessions, bounded ordinary input plus advancement, captures, surveys, recordings and evidence |
| Tester | Choose goals and actions, interpret results, investigate friction and report uncertainty |

Use the existing packaged SDK/runtime and generated debug catalog. The Engine
already supplies time gating and the browser adapter. A product adapter should
read the real game state and use the normal input path. It needs no local clock,
shadow gameplay state, browser gameplay loop, handwritten native bridge, replay,
or alternate collision/pathfinding implementation.

## Start with observation, actions and look

Register `Rusty.Engine.Debugging.PlaytestDebugModule` through the product's existing
`RegisterDebugCommands(IDebugCommandModuleRegistrar registrar)` hook. This schematic
uses callbacks implemented by the game's current session:

```csharp
registrar.Register(new PlaytestDebugModule(
    session.ReadPlaytestObservation,
    session.InspectAction,
    new[] { "forward", "back", "left", "right", "use", "attack" },
    session.InspectLook));
```

Callback contracts:

- Observation: `Func<DebugCommandResult>`, returning the current compact facts.
- Action: `Func<string, PlaytestAction>`, resolving from live gameplay state.
- Look: `Func<double, double, DebugCommandResult>`, relative yaw/pitch degrees
  through the game's existing look rules, without ticking gameplay.

`PlaytestAction` carries an ID, ordinary physical key code, duration in ms,
held-versus-tap behavior, availability/reason, and optional equipment/held keys.
Query current equipment and animation/recovery timing each time. Report cooldown,
ammo, grounded/dead state or other relevant refusal through existing game rules.
The normal input handler still decides whether the action executes. Keep action
windows bounded; the current Crew `act` operation accepts at most 2000 ms.

For observations, expose enough information to make the next decision:

- World axes/units, player position and whether it is body center or feet,
  facing/pitch, floor height and grounded state.
- Health/resources, current equipment, cooldown and action availability.
- Nearby named targets with stable IDs and current states, including defeated
  enemies when relevant. Make completed/dead/inactive states distinguishable.
- Interaction focus and refusal: reach, visibility/occlusion, alignment or policy.
- Last movement result and recent damage source when available.

Reuse existing gameplay facts/receipts. A generation or simulation-step stamp
helps interpret a sample; it is not proof of exact screenshot synchronization.
Keep common observations compact and offer larger diagnostics on request.

## Add spatial and interaction tools as needed

| Need | Existing Engine surface | Product supplies |
| --- | --- | --- |
| Local occupancy | `SpatialInspectionDebugModule`, `SpatialGridSnapshot.Capture` | Current spatial session, grid bounds, relevant dynamic colliders |
| Nearby bumps | `PlaytestTraversal.Probe` | Feet, body/step height, query distance; pair with current movement receipt |
| Body clearance | `SpatialClearanceDebugModule`, `SpatialClearanceSnapshot.Capture` | Body center/height, target feet, live controller config and dynamic colliders |
| Physical jump hint | `PlaytestTraversal.JumpToward` via inspection module | Live tuning, grounded/eligible state, target feet and ordinary jump/forward bindings |
| Door/use alignment | `InteractionDebugModule` | Existing interaction service's current candidates and rules |
| Goals/routes | Optional `navigation.targets` / `navigation.route` | Product target meaning and existing Engine navigation queries |

Use the same authoritative collider records that normal movement uses, including
current door states. Document coverage: Doom's supplied dynamic colliders are
current doors, not enemies or pickups. A collision grid is a sampled occupancy
view; it does not need a second voxel world. Empty cells do not prove walkability.

Clearance separates current overlap, direct sweep, target overlap and support
below the target. Floor contact at the start of a sweep does not automatically
mean an obstacle ahead. Preserve normal/source and query status. A support normal
within the slope limit does not guarantee a landing. Jump guidance estimates an
ordinary input window; inspect the resulting pose and grounded state.

For true 3D games, keep full XYZ/vertical and slope information, use actual body
shape/tuning, and expose relevant gameplay state such as stance or movement mode.
Enable only helpers whose assumptions fit that controller. Extend shared Engine
mechanisms upstream when needed; adapt game-specific eligibility and targets in
the product. Doom's current capsule/jump helpers are not a universal controller.

## Host setup and a small acceptance exercise

1. Consume a matching SDK/runtime pair containing the modules and enable the
   ordinary live-debug option on the product host. Doom currently uses local
   development artifacts; don't copy its dated paths as a portable dependency.
2. Register a local browser profile with controls and explicit reset/shared-host
   semantics. Set `interaction_queries: true` when exposing those queries. Reload
   profiles through `playtest reload`, preserving active sessions.
3. Discover available operations and capture the ready game. Switch to
   action-driven time, look around, then execute one short ordinary movement.
4. Confirm actual displacement; query a live equipment action and perform it.
   Open/interact with one object and inspect the real outcome.
5. If spatial helpers are enabled, inspect a real bump, compare time before/after
   reads, and try one ordinary recovery. A successful query alone is not a
   successful traversal. Document missing collider coverage or query ambiguity.
6. Capture the result and an exit interview. Record source/SDK/runtime identities
   and separate product behavior from service readiness. Verify only the changed
   behavior; no exact-time certification or full scripted playthrough is needed.

## Source map

These paths are relative to sibling repository roots:

- `rusty-engine/docs/playtest-inspection.md`: Engine contract and ownership.
- `rusty-engine/csharp/Rusty.Engine/Debugging/PlaytestDebugModule.cs`: exact action
  record, callbacks and generated commands.
- `rusty-engine/csharp/Rusty.Engine/Debugging/SpatialClearanceSnapshot.cs`: capsule
  helper and registration contract.
- `rusty-doom/csharp/LoadingBay.Game/LoadingBayProduct.cs`: module registration.
- `rusty-doom/csharp/LoadingBay.Game/LoadingBayRoomStudy.cs`: compact observation,
  live action resolver and look adapter over current gameplay.
- `rusty-doom/csharp/LoadingBay.Game/LoadingBayPlaytestSpatial.cs`: triggered spatial
  delegates using public SDK helpers and current player state.
- `rusty-doom/csharp/LoadingBay.Game/LoadingBayNavigationGuidance.cs`: product goals
  and route hints using existing Engine mechanisms.
- `rusty-doom/docs/playtest-inspection.md`: coverage and development setup.

Read the current files when implementing; use their ownership pattern and public
API, rather than copying Doom's game rules or dependency snapshot.
