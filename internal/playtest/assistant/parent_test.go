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

type navigationEnv struct {
	fakeEnv
	target          string
	observedTargets []string
}

func (e *navigationEnv) Observe(context.Context) (Observation, error) {
	e.observations++
	e.observedTargets = append(e.observedTargets, e.target)
	progress, _ := json.Marshal(map[string]int{"progress": e.inputs})
	targets := json.RawMessage(`{"targets":[{"id":"alpha","label":"Alpha"},{"id":"beta","label":"Beta"}]}`)
	route, _ := json.Marshal(map[string]any{"target": e.target, "waypoint": map[string]int{"x": e.inputs}, "distance": 3, "bearing": 0, "status": "reachable"})
	return Observation{CapturedAt: time.Now(), NavigationTarget: e.target, Facts: map[string]json.RawMessage{"game": progress, "navigation.targets": targets, "navigation.route": route}}, nil
}
func (e *navigationEnv) SetNavigationTarget(target string) error {
	e.target = target
	return nil
}
func (e *navigationEnv) NavigationEnabled() bool { return true }
func (e *navigationEnv) Input(ctx context.Context, steps []map[string]any) (json.RawMessage, error) {
	time.Sleep(time.Millisecond)
	return e.fakeEnv.Input(ctx, steps)
}

type namedParent struct{ target string }

func (p namedParent) Advise(context.Context, State, []Tactic) (ParentDecision, error) {
	return ParentDecision{Guidance: Guidance{Objective: "Move toward selected target", NavigationTarget: p.target}}, nil
}

type navigationController struct {
	pairs [][2]string
}

func (c *navigationController) Decide(_ context.Context, state State, _ []Tactic) (Decision, error) {
	if state.Guidance != nil {
		var route struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(state.Observation.Facts["navigation.route"], &route)
		c.pairs = append(c.pairs, [2]string{state.Guidance.NavigationTarget, route.Target})
	}
	return Decision{Choice: "forward", Confidence: 1}, nil
}

func TestNavigationTargetReplacementWaitsForFreshMatchingRoute(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 4
	p.Parent = &ParentPolicy{EveryActions: 10, TimeoutMS: 500, MaxAgeMS: 500}
	env := &navigationEnv{target: "alpha"}
	controller := &navigationController{}
	var log strings.Builder
	result, err := RunWithParent(context.Background(), p, env, controller, namedParent{target: "beta"}, &log)
	if err != nil || len(controller.pairs) == 0 {
		t.Fatalf("%+v %v pairs=%v\n%s", result, err, controller.pairs, log.String())
	}
	for _, pair := range controller.pairs {
		if pair[0] != pair[1] {
			t.Fatalf("guidance target %q paired with route target %q\n%s", pair[0], pair[1], log.String())
		}
	}
	if env.target != "beta" || !strings.Contains(log.String(), `"deferred_for_navigation_observation"`) || !strings.Contains(log.String(), `"navigation_guidance_adopted"`) {
		t.Fatalf("target=%q\n%s", env.target, log.String())
	}
}

func TestInvalidNavigationTargetDoesNotReplacePriorGuidance(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 8
	p.Parent = &ParentPolicy{EveryActions: 1, TimeoutMS: 500, MaxAgeMS: 500}
	env := &navigationEnv{target: "alpha"}
	controller := &navigationController{}
	parent := &sequenceParent{targets: []string{"alpha", "missing"}}
	var log strings.Builder
	result, err := RunWithParent(context.Background(), p, env, controller, parent, &log)
	if err != nil || !strings.Contains(log.String(), `"invalid_navigation_target"`) {
		t.Fatalf("%+v %v\n%s", result, err, log.String())
	}
	for _, pair := range controller.pairs {
		if pair[0] != "alpha" || pair[1] != "alpha" {
			t.Fatalf("invalid update replaced prior guidance: %v\n%s", controller.pairs, log.String())
		}
	}
}

type stopParent struct{}

func (stopParent) Advise(context.Context, State, []Tactic) (ParentDecision, error) {
	return ParentDecision{Guidance: Guidance{Objective: "Hand back", NavigationTarget: "beta", Stop: true}}, nil
}

func TestParentStopWithChangedNavigationTargetDoesNotRefreshRouteOrInputAgain(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 4
	p.Parent = &ParentPolicy{EveryActions: 10, TimeoutMS: 500, MaxAgeMS: 500}
	env := &navigationEnv{target: "alpha"}
	var log strings.Builder
	result, err := RunWithParent(context.Background(), p, env, &Baseline{}, stopParent{}, &log)
	if err != nil || result.Reason != "parent_requested_handback" || env.inputs != 1 {
		t.Fatalf("%+v %v inputs=%d\n%s", result, err, env.inputs, log.String())
	}
	if env.target != "alpha" || strings.Contains(log.String(), `"deferred_for_navigation_observation"`) {
		t.Fatalf("target=%q observed=%v\n%s", env.target, env.observedTargets, log.String())
	}
	for _, target := range env.observedTargets {
		if target != "alpha" {
			t.Fatalf("queried route for stopped destination: %v", env.observedTargets)
		}
	}
}

type sequenceParent struct {
	targets []string
	n       int
}

func (p *sequenceParent) Advise(context.Context, State, []Tactic) (ParentDecision, error) {
	target := p.targets[p.n]
	if p.n < len(p.targets)-1 {
		p.n++
	}
	return ParentDecision{Guidance: Guidance{Objective: "Keep target", NavigationTarget: target}}, nil
}

func TestParentTargetWithoutRouteAssistanceStaysReadOnly(t *testing.T) {
	p := policy()
	p.StopWhen = nil
	p.MaxActions = 4
	p.Parent = &ParentPolicy{EveryActions: 10, TimeoutMS: 500, MaxAgeMS: 500}
	env := &targetOnlyEnv{}
	controller := &guidanceController{}
	var log strings.Builder
	result, err := RunWithParent(context.Background(), p, env, controller, namedParent{target: "alpha"}, &log)
	if err != nil || !controller.seen || strings.Contains(log.String(), `"navigation.route"`) {
		t.Fatalf("%+v %v seen=%v\n%s", result, err, controller.seen, log.String())
	}
}

type targetOnlyEnv struct{ fakeEnv }

func (e *targetOnlyEnv) Observe(context.Context) (Observation, error) {
	e.observations++
	progress, _ := json.Marshal(map[string]int{"progress": e.inputs})
	return Observation{CapturedAt: time.Now(), Facts: map[string]json.RawMessage{
		"game":               progress,
		"navigation.targets": json.RawMessage(`{"targets":[{"id":"alpha","label":"Alpha"}]}`),
	}}, nil
}
func (e *targetOnlyEnv) Input(ctx context.Context, steps []map[string]any) (json.RawMessage, error) {
	time.Sleep(time.Millisecond)
	return e.fakeEnv.Input(ctx, steps)
}
