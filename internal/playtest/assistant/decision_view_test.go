package assistant

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDecisionViewStateSelectsMovementAndHealthDelta(t *testing.T) {
	previousAt := time.Now().UTC().Add(-time.Second)
	currentAt := previousAt.Add(250 * time.Millisecond)
	state := State{
		Goal:          "reach the door",
		Instructions:  "keep clear of enemies",
		Observation:   decisionObservation(currentAt, `{"pose":{"x":4,"y":2,"z":8},"health":87,"weapon":"shotgun","enemies":[{"id":"trooper-1","live":true},null],"map":{"cells":[1,2]},"screen":"/tmp/current.png"}`),
		Previous:      decisionObservationPointer(decisionObservation(previousAt, `{"pose":{"x":1,"y":2,"z":4},"health":100,"map":{"cells":[3]}}`)),
		LastAction:    "forward",
		RecentActions: []string{"turn", "forward"},
		ActionsTaken:  2,
		RemainingMS:   1200,
	}
	view := &DecisionView{
		Fields: map[string]string{
			"health":  "/facts/combat/health",
			"weapon":  "/facts/combat/weapon",
			"enemies": "/facts/combat/enemies",
		},
		Deltas:    map[string]string{"health": "/facts/combat/health"},
		Positions: map[string]string{"player": "/facts/combat/pose"},
	}

	raw, err := json.Marshal(view.State(state))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	observation := got["observation"].(map[string]any)
	if observation["captured_at"] != currentAt.Format(time.RFC3339Nano) || observation["sample_interval_ms"] != float64(250) {
		t.Fatalf("observation timing = %#v", observation)
	}
	if age, ok := observation["previous_observation_age_ms"].(float64); !ok || age < 1000 {
		t.Fatalf("previous observation age = %#v", observation["previous_observation_age_ms"])
	}
	if fields := observation["fields"].(map[string]any); fields["health"] != float64(87) || fields["weapon"] != "shotgun" {
		t.Fatalf("fields = %#v", fields)
	}
	enemies := observation["fields"].(map[string]any)["enemies"].([]any)
	if len(enemies) != 2 || enemies[1] != nil || enemies[0].(map[string]any)["id"] != "trooper-1" {
		t.Fatalf("structured selected field = %#v", enemies)
	}
	if delta := observation["deltas"].(map[string]any)["health"]; delta != float64(-13) {
		t.Fatalf("health delta = %#v", delta)
	}
	position := observation["positions"].(map[string]any)["player"].(map[string]any)
	if position["dx"] != float64(3) || position["dy"] != float64(0) || position["dz"] != float64(4) || position["displacement"] != float64(5) {
		t.Fatalf("position = %#v", position)
	}
	if _, hasCapture := observation["capture"]; hasCapture || string(raw) == "" || containsJSONKey(raw, "facts") || containsJSONKey(raw, "previous") || containsJSONKey(raw, "map") || containsJSONKey(raw, "screen") {
		t.Fatalf("compact decision state leaked raw observation: %s", raw)
	}
}

func TestDecisionViewUnknownIsNotZeroAndDoesNotMutateState(t *testing.T) {
	currentAt := time.Now().UTC()
	state := State{
		Observation:   decisionObservation(currentAt, `{"pose":{"x":0,"y":0,"z":0},"health":0}`),
		RecentActions: []string{"forward"},
	}
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	view := &DecisionView{
		Fields: map[string]string{
			"health":  "/facts/combat/health",
			"missing": "/facts/combat/missing",
		},
		Deltas:    map[string]string{"health": "/facts/combat/health", "missing": "/facts/combat/missing"},
		Positions: map[string]string{"player": "/facts/combat/pose", "missing": "/facts/combat/missing"},
	}

	got := view.State(state).(map[string]any)
	observation := got["observation"].(map[string]any)
	fields := observation["fields"].(map[string]any)
	if fields["health"] != float64(0) || fields["missing"] != unknownDecisionFact {
		t.Fatalf("fields = %#v", fields)
	}
	deltas := observation["deltas"].(map[string]any)
	if deltas["health"] != unknownDecisionFact || deltas["missing"] != unknownDecisionFact {
		t.Fatalf("deltas = %#v", deltas)
	}
	positions := observation["positions"].(map[string]any)
	player := positions["player"].(map[string]any)
	if player["x"] != float64(0) || player["dx"] != unknownDecisionFact || player["displacement"] != unknownDecisionFact || positions["missing"] != unknownDecisionFact {
		t.Fatalf("positions = %#v", positions)
	}
	if observation["previous_observation_age_ms"] != unknownDecisionFact || observation["sample_interval_ms"] != unknownDecisionFact {
		t.Fatalf("missing previous timing = %#v", observation)
	}
	after, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("State mutated\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestDecisionViewValidationAndNilFallback(t *testing.T) {
	if err := (&DecisionView{Fields: map[string]string{"bad": "/capture/path"}}).Validate(); err == nil {
		t.Fatal("accepted screenshot pointer")
	}
	if err := (&DecisionView{Fields: map[string]string{"bad": "facts/value"}}).Validate(); err == nil {
		t.Fatal("accepted non-pointer")
	}
	var view *DecisionView
	state := State{Observation: Observation{Capture: json.RawMessage(`{"path":"/tmp/evidence.png"}`)}}
	got := view.State(state).(State)
	if got.Observation.Capture != nil {
		t.Fatalf("nil view did not retain semantic state behavior: %#v", got)
	}
}

func decisionObservation(capturedAt time.Time, fact string) Observation {
	return Observation{CapturedAt: capturedAt, Capture: json.RawMessage(`{"path":"/tmp/evidence.png"}`), Facts: map[string]json.RawMessage{"combat": json.RawMessage(fact)}}
}

func decisionObservationPointer(observation Observation) *Observation { return &observation }

func containsJSONKey(raw []byte, key string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return containsKey(value, key)
}

func containsKey(value any, key string) bool {
	switch value := value.(type) {
	case map[string]any:
		for childKey, child := range value {
			if childKey == key || containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}
