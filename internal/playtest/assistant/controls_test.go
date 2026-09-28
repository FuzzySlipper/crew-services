package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func groupFixture() ([]ControlGroup, []Tactic) {
	return []ControlGroup{{ID: "movement", Tactics: []string{"forward"}}, {ID: "look", Tactics: []string{"right"}}, {ID: "trigger", Tactics: []string{"fire"}}}, []Tactic{
		{ID: "forward", Description: "forward", Steps: []map[string]any{{"kind": "gamepad", "ms": 150, "ly": 1.0}}},
		{ID: "right", Description: "right", Steps: []map[string]any{{"kind": "gamepad", "ms": 150, "rx": .4}}},
		{ID: "fire", Description: "fire", Steps: []map[string]any{{"kind": "gamepad", "ms": 150, "rt": 1.0}}},
	}
}
func TestControlGroupsComposeAndRejectConflicts(t *testing.T) {
	g, ts := groupFixture()
	if err := validateGroups(g, ts); err != nil {
		t.Fatal(err)
	}
	x, err := composeControls(g, ts, map[string]string{"movement": "forward", "look": "right", "trigger": "fire"})
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Steps) != 1 || x.Steps[0]["ly"] != 1.0 || x.Steps[0]["rx"] != .4 || x.Steps[0]["rt"] != 1.0 {
		t.Fatal(x)
	}
	if _, err = composeControls(g, ts, map[string]string{"movement": "fire", "look": "right", "trigger": "fire"}); err == nil {
		t.Fatal("cross-group option accepted")
	}
	ts[1].Steps[0]["ly"] = -1.0
	if validateGroups(g, ts) == nil {
		t.Fatal("overlapping fields accepted")
	}
	delete(ts[1].Steps[0], "ly")
	ts[1].Steps[0]["ms"] = 100
	if validateGroups(g, ts) == nil {
		t.Fatal("unequal holds accepted")
	}
}
func TestJevComposedQuestionsPreserveAllProbabilities(t *testing.T) {
	g, ts := groupFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		q := body["questions"].(map[string]any)
		if len(q) != 4 || q["next_action"] != nil {
			t.Error(q)
		}
		w.Write([]byte(`{"answers":{"status":{"type":"choice","choice":"continue","confidence":0.9},"movement":{"type":"choice","choice":"forward","confidence":0.8,"probabilities":{"forward":0.9}},"look":{"type":"choice","choice":"right","confidence":0.7},"trigger":{"type":"choice","choice":"fire","confidence":0.6}}}`))
	}))
	defer server.Close()
	j := Jev{BaseURL: server.URL, Groups: g}
	d, err := j.Decide(context.Background(), State{}, ts)
	if err != nil || d.Confidence != .6 || d.Choices["look"] != "right" || d.Probabilities["movement"]["forward"] != .9 {
		t.Fatalf("%+v %v", d, err)
	}
}

type oldEnv struct{ fakeEnv }

func (f *oldEnv) Observe(ctx context.Context) (Observation, error) {
	o, e := f.fakeEnv.Observe(ctx)
	o.CapturedAt = time.Now().Add(-time.Second)
	return o, e
}
func TestStaleDecisionNeverDispatches(t *testing.T) {
	p := policy()
	p.BudgetMS = 100
	p.DecisionMaxAgeMS = 100
	f := &oldEnv{}
	var log strings.Builder
	r, e := Run(context.Background(), p, f, &Baseline{}, &log)
	if e != nil || f.inputs != 0 || !f.cancelled || r.Reason != "deadline_or_cancelled" || !strings.Contains(log.String(), "decision_discarded") {
		t.Fatalf("%+v %v inputs%d", r, e, f.inputs)
	}
}
func TestBudgetDoesNotDispatchHoldThatCannotFinish(t *testing.T) {
	p := policy()
	p.BudgetMS = 100
	p.Tactics[0].Steps[0]["ms"] = 150
	f := &fakeEnv{}
	var log strings.Builder
	r, e := Run(context.Background(), p, f, &Baseline{}, &log)
	if e != nil || f.inputs != 0 || !f.cancelled || r.Reason != "deadline_or_cancelled" {
		t.Fatalf("%+v %v", r, e)
	}
}

type deadlineObservationEnv struct{ fakeEnv }

func (f *deadlineObservationEnv) Observe(ctx context.Context) (Observation, error) {
	<-ctx.Done()
	return Observation{}, ctx.Err()
}
func TestObservationInterruptedByBudgetIsLabeledDeadline(t *testing.T) {
	p := policy()
	p.BudgetMS = 100
	f := &deadlineObservationEnv{}
	var log strings.Builder
	r, e := Run(context.Background(), p, f, &Baseline{}, &log)
	if e != nil || r.Reason != "deadline_or_cancelled" || r.Error == "" || f.inputs != 0 || !f.cancelled {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestInvalidStatusConfidenceNeverDispatchesGroupedControls(t *testing.T) {
	g, ts := groupFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"status":{"type":"choice","choice":"continue","confidence":2},"movement":{"type":"choice","choice":"forward","confidence":0.8},"look":{"type":"choice","choice":"right","confidence":0.8},"trigger":{"type":"choice","choice":"fire","confidence":0.8}}}`))
	}))
	defer server.Close()
	p := policy()
	p.ControlGroups = g
	p.Tactics = ts
	f := &fakeEnv{}
	var log strings.Builder
	r, e := Run(context.Background(), p, f, &Jev{BaseURL: server.URL, Groups: g}, &log)
	if e != nil || r.Reason != "controller_failed" || !strings.Contains(r.Error, "status confidence") || f.inputs != 0 || !f.cancelled {
		t.Fatalf("%+v %v inputs%d", r, e, f.inputs)
	}
}
