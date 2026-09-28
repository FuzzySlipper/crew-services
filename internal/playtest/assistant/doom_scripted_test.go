package assistant

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDoomScriptedBlockedAssistedHitDoesNotFire(t *testing.T) {
	controller := &DoomScripted{}
	state := doomScriptedState(t, 4, doomCombatFixture(4, 0, 0, true, "StaticMesh", 4, "Entity", 4))
	decision, err := controller.Decide(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Choices[doomTriggerGroup] != doomTriggerNone {
		t.Fatalf("blocked assisted ray fired: %+v", decision)
	}
}

func TestDoomScriptedTurnsWithBearingSign(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bearing float64
		want    string
	}{
		{name: "right", bearing: 12, want: doomTurnRight},
		{name: "left", bearing: -12, want: doomTurnLeft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := (&DoomScripted{}).Decide(context.Background(), doomScriptedState(t, 1, doomCombatFixture(20, tc.bearing, 0, false, "None", nil, "None", nil)), nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Choices[doomLookGroup] != tc.want {
				t.Fatalf("got %+v, want %s", decision, tc.want)
			}
		})
	}
}

func TestDoomScriptedScansRightWhileNoTargetIsVisible(t *testing.T) {
	fact := doomCombatFixture(20, 0, 0, false, "None", nil, "None", nil)
	fact["enemies"] = []any{}
	controller := &DoomScripted{}
	for action := 0; action < 3; action++ {
		decision, err := controller.Decide(context.Background(), doomScriptedState(t, action, fact), nil)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Choices[doomMovementGroup] != doomMoveForward || decision.Choices[doomLookGroup] != doomTurnRight {
			t.Fatalf("action %d did not keep scanning right: %+v", action, decision)
		}
	}
}

func TestDoomScriptedRecoversOnlyAfterBlockedForwardMovement(t *testing.T) {
	blocked := &DoomScripted{}
	current := doomCombatFixture(20, 0, 0, false, "None", nil, "None", nil)
	previous := doomCombatFixture(20, 0, 0, false, "None", nil, "None", nil)
	for i := 0; i < doomBlockedForwardCycles; i++ {
		state := doomScriptedState(t, i, current)
		state.Previous = doomObservationPointer(t, previous)
		state.LastAction = "movement=forward;look=look_none;trigger=trigger_none"
		decision, err := blocked.Decide(context.Background(), state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i < doomBlockedForwardCycles-1 && decision.Choices[doomMovementGroup] != doomMoveForward {
			t.Fatalf("recovered too early: %+v", decision)
		}
		if i == doomBlockedForwardCycles-1 && decision.Choices[doomMovementGroup] != doomStrafeLeft {
			t.Fatalf("did not begin recovery: %+v", decision)
		}
	}

	turning := &DoomScripted{}
	for i := 0; i < doomBlockedForwardCycles+2; i++ {
		state := doomScriptedState(t, i, current)
		state.Previous = doomObservationPointer(t, previous)
		state.LastAction = "movement=move_none;look=turn_right;trigger=trigger_none"
		decision, err := turning.Decide(context.Background(), state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Choices[doomMovementGroup] != doomMoveForward {
			t.Fatalf("stationary turn was falsely recovered: %+v", decision)
		}
	}
}

func doomScriptedState(t *testing.T, actions int, fact map[string]any) State {
	t.Helper()
	raw, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	return State{ActionsTaken: actions, Observation: Observation{CapturedAt: time.Now(), Facts: map[string]json.RawMessage{"combat.observe": raw}}}
}

func doomObservationPointer(t *testing.T, fact map[string]any) *Observation {
	state := doomScriptedState(t, 0, fact)
	return &state.Observation
}

func doomCombatFixture(distance, bearing, pitch float64, assistActive bool, assistedKind string, assistedEntity any, rawKind string, rawEntity any) map[string]any {
	return map[string]any{
		"player": map[string]any{
			"position":    map[string]any{"x": 1.0, "z": 2.0},
			"weaponReady": true,
			"aimAssist": map[string]any{
				"active": assistActive,
				"shot": map[string]any{
					"assistedHit": map[string]any{"kind": assistedKind, "entity": assistedEntity},
					"rawHit":      map[string]any{"kind": rawKind, "entity": rawEntity},
				},
			},
		},
		"enemies": []any{map[string]any{
			"id":                   4,
			"health":               20,
			"distance":             distance,
			"bearingDegrees":       bearing,
			"aimPitchErrorDegrees": pitch,
			"lineOfSight":          true,
		}},
	}
}
