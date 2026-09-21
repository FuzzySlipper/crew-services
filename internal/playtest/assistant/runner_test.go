package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeEnv struct {
	observations int
	inputs       int
	cancelled    bool
	inputErr     bool
	fixed        bool
}

func (f *fakeEnv) Observe(context.Context) (Observation, error) {
	f.observations++
	n := f.inputs
	if f.fixed {
		n = 0
	}
	raw, _ := json.Marshal(map[string]int{"progress": n})
	return Observation{CapturedAt: time.Now(), Facts: map[string]json.RawMessage{"game": raw}}, nil
}
func (f *fakeEnv) Input(context.Context, []map[string]any) (json.RawMessage, error) {
	f.inputs++
	if f.inputErr {
		return nil, errors.New("delivery unknown")
	}
	return json.RawMessage(`{"completed_steps":1}`), nil
}
func (f *fakeEnv) Cancel(ctx context.Context) (json.RawMessage, error) {
	f.cancelled = ctx.Err() == nil
	return json.RawMessage(`{"released":true}`), nil
}
func policy() Policy {
	return Policy{Goal: "Move until progress reaches two", BudgetMS: 1000, MaxActions: 5, DecisionTimeoutMS: 100, MinimumConfidence: .7, StallActions: 2, ProgressPointers: []string{"/facts/game/progress"}, StopWhen: []Condition{{Pointer: "/facts/game/progress", Operator: "gte", Value: float64(2), Reason: "goal_observed"}}, Tactics: []Tactic{{ID: "forward", Description: "move forward", Steps: []map[string]any{{"kind": "hold", "keys": []int{87}, "ms": 10}}}}}
}
func TestObservedCompletionAndTranscript(t *testing.T) {
	f := &fakeEnv{}
	var log strings.Builder
	r, e := Run(context.Background(), policy(), f, &Baseline{}, &log)
	if e != nil || r.Reason != "goal_observed" || f.inputs != 2 || !f.cancelled {
		t.Fatalf("%+v %v %+v", r, e, f)
	}
	if !strings.Contains(log.String(), "controller_inference") || !strings.Contains(log.String(), "handback") {
		t.Fatal(log.String())
	}
}
func TestStallAndUnknownInputNeverReplay(t *testing.T) {
	for _, tc := range []struct {
		f      fakeEnv
		reason string
	}{{fakeEnv{fixed: true}, "stalled_observed_facts"}, {fakeEnv{inputErr: true}, "input_delivery_uncertain"}} {
		f := tc.f
		var log strings.Builder
		r, e := Run(context.Background(), policy(), &f, &Baseline{}, &log)
		if e != nil || r.Reason != tc.reason || !f.cancelled {
			t.Fatalf("%+v %v", r, e)
		}
		if f.inputErr && f.inputs != 1 {
			t.Fatal("replayed")
		}
	}
}

type slowController struct{}

func (slowController) Decide(ctx context.Context, _ State, _ []Tactic) (Decision, error) {
	<-ctx.Done()
	return Decision{}, ctx.Err()
}
func TestDecisionTimeoutReleasesWithFreshContext(t *testing.T) {
	f := &fakeEnv{}
	var log strings.Builder
	start := time.Now()
	r, e := Run(context.Background(), policy(), f, slowController{}, &log)
	if e != nil || r.Reason != "controller_failed" || !f.cancelled || f.inputs != 0 || time.Since(start) > time.Second {
		t.Fatalf("%+v %v", r, e)
	}
}

type chosen string

func (c chosen) Decide(context.Context, State, []Tactic) (Decision, error) {
	return Decision{Choice: string(c), Confidence: 1}, nil
}
func TestModelCompletionRemainsInference(t *testing.T) {
	f := &fakeEnv{}
	var log strings.Builder
	r, _ := Run(context.Background(), policy(), f, chosen("goal_complete"), &log)
	if r.Reason != "controller_goal_complete" || f.inputs != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestCancelledIntervalDoesNotAct(t *testing.T) {
	f := &fakeEnv{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var log strings.Builder
	r, e := Run(ctx, policy(), f, &Baseline{}, &log)
	if e != nil || r.Reason != "deadline_or_cancelled" || f.inputs != 0 || !f.cancelled {
		t.Fatalf("%+v %v %+v", r, e, f)
	}
}
func TestUnknownModelChoiceDoesNotAct(t *testing.T) {
	f := &fakeEnv{}
	var log strings.Builder
	r, _ := Run(context.Background(), policy(), f, chosen("teleport"), &log)
	if r.Reason != "invalid_decision" || f.inputs != 0 || !f.cancelled {
		t.Fatalf("%+v", r)
	}
}
