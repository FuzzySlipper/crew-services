package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type concurrentEnv struct {
	fakeEnv
	reached chan struct{}
}

func (e *concurrentEnv) Input(ctx context.Context, steps []map[string]any) (json.RawMessage, error) {
	time.Sleep(3 * time.Millisecond)
	r, err := e.fakeEnv.Input(ctx, steps)
	if e.inputs == 3 {
		close(e.reached)
	}
	return r, err
}

type waitingParent struct{ reached chan struct{} }

func (p waitingParent) Advise(ctx context.Context, _ State, _ []Tactic) (ParentDecision, error) {
	select {
	case <-p.reached:
		return ParentDecision{Guidance: Guidance{Objective: "Adjust engagement distance", Parameters: map[string]any{"distance": 6}}}, nil
	case <-ctx.Done():
		return ParentDecision{}, ctx.Err()
	}
}

type guidanceController struct{ seen bool }

func (c *guidanceController) Decide(_ context.Context, s State, _ []Tactic) (Decision, error) {
	if s.Guidance != nil {
		c.seen = true
	}
	return Decision{Choice: "forward", Confidence: 1}, nil
}
func TestParentDoesNotBlockActionsAndAppliesAtBoundary(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 8
	p.Parent = &ParentPolicy{EveryActions: 10, TimeoutMS: 500, MaxAgeMS: 500}
	e := &concurrentEnv{reached: make(chan struct{})}
	controller := &guidanceController{}
	var log strings.Builder
	r, err := RunWithParent(context.Background(), p, e, controller, waitingParent{e.reached}, &log)
	if err != nil || !controller.seen || r.Actions != 8 {
		t.Fatalf("%+v %v guidance=%v\n%s", r, err, controller.seen, log.String())
	}
	if !strings.Contains(log.String(), `"actions_while_pending":`) {
		t.Fatal(log.String())
	}
}

type expiredEnv struct{ concurrentEnv }

func (e *expiredEnv) Observe(ctx context.Context) (Observation, error) {
	o, err := e.fakeEnv.Observe(ctx)
	o.CapturedAt = time.Now().Add(-time.Second)
	return o, err
}
func TestStaleParentUpdateDoesNotReplaceGuidance(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 8
	p.Parent = &ParentPolicy{EveryActions: 10, TimeoutMS: 500, MaxAgeMS: 100}
	e := &expiredEnv{concurrentEnv: concurrentEnv{reached: make(chan struct{})}}
	controller := &guidanceController{}
	var log strings.Builder
	r, err := RunWithParent(context.Background(), p, e, controller, waitingParent{e.reached}, &log)
	if err != nil || controller.seen || r.Actions != 8 || !strings.Contains(log.String(), `"disposition":"stale"`) {
		t.Fatalf("%+v %v seen=%v\n%s", r, err, controller.seen, log.String())
	}
}
