// Package assistant implements bounded semantic playtesting outside the messaging core.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"crew-services/internal/playtest/target"
)

type Tactic struct {
	ID          string           `json:"id"`
	Description string           `json:"description"`
	Steps       []map[string]any `json:"steps"`
}
type Condition struct {
	Pointer  string `json:"pointer"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
	Reason   string `json:"reason"`
}
type Policy struct {
	Goal              string      `json:"goal"`
	Instructions      string      `json:"instructions"`
	BudgetMS          int         `json:"budget_ms"`
	MaxActions        int         `json:"max_actions"`
	DecisionTimeoutMS int         `json:"decision_timeout_ms"`
	MinimumConfidence float64     `json:"minimum_confidence"`
	StallActions      int         `json:"stall_actions"`
	ProgressPointers  []string    `json:"progress_pointers"`
	StopWhen          []Condition `json:"stop_when"`
	Tactics           []Tactic    `json:"tactics"`
}
type Decision struct {
	Choice     string          `json:"choice"`
	Confidence float64         `json:"confidence"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}
type State struct {
	Goal         string       `json:"goal"`
	Instructions string       `json:"instructions"`
	Observation  Observation  `json:"observation"`
	Previous     *Observation `json:"previous,omitempty"`
	LastAction   string       `json:"last_action,omitempty"`
	ActionsTaken int          `json:"actions_taken"`
	RemainingMS  int64        `json:"remaining_ms"`
}
type Controller interface {
	Decide(context.Context, State, []Tactic) (Decision, error)
}
type Environment interface {
	Observe(context.Context) (Observation, error)
	Input(context.Context, []map[string]any) (json.RawMessage, error)
	Cancel(context.Context) (json.RawMessage, error)
}
type Result struct {
	Reason       string          `json:"reason"`
	Error        string          `json:"error,omitempty"`
	Actions      int             `json:"actions"`
	ElapsedMS    int64           `json:"elapsed_ms"`
	Current      *Observation    `json:"current,omitempty"`
	Cleanup      json.RawMessage `json:"cleanup,omitempty"`
	CleanupError string          `json:"cleanup_error,omitempty"`
}

func (p Policy) Validate() error {
	if strings.TrimSpace(p.Goal) == "" || p.BudgetMS < 100 || p.BudgetMS > 120000 || p.MaxActions < 1 || p.MaxActions > 100 || p.DecisionTimeoutMS < 100 || p.DecisionTimeoutMS > 30000 {
		return errors.New("goal and bounded budget_ms(100..120000), max_actions(1..100), decision_timeout_ms(100..30000) required")
	}
	if math.IsNaN(p.MinimumConfidence) || p.MinimumConfidence < 0 || p.MinimumConfidence > 1 {
		return errors.New("minimum_confidence must be 0..1")
	}
	if p.StallActions < 1 || len(p.ProgressPointers) == 0 {
		return errors.New("stall_actions and progress_pointers required")
	}
	ids := map[string]bool{"goal_complete": true, "stalled": true, "unexpected_state": true, "uncertain": true}
	if len(p.Tactics) == 0 || len(p.Tactics) > 24 {
		return errors.New("1..24 tactics required")
	}
	for _, t := range p.Tactics {
		if t.ID == "" || ids[t.ID] || t.Description == "" || len(t.Steps) == 0 {
			return fmt.Errorf("invalid or duplicate tactic %q", t.ID)
		}
		ids[t.ID] = true
		if err := target.ValidateBatch(t.Steps); err != nil {
			return fmt.Errorf("tactic %s: %w", t.ID, err)
		}
		raw, _ := json.Marshal(t.Steps)
		var steps []struct {
			MS int `json:"ms"`
		}
		json.Unmarshal(raw, &steps)
		total := 0
		for _, s := range steps {
			total += s.MS
		}
		if total > 2000 {
			return errors.New("each tactic must last at most 2000ms")
		}
	}
	for _, c := range p.StopWhen {
		if c.Pointer == "" || c.Reason == "" {
			return errors.New("stop conditions need pointer and reason")
		}
		switch c.Operator {
		case "eq", "lte", "gte":
		default:
			return errors.New("condition operator must be eq, lte or gte")
		}
	}
	return nil
}

// Run returns control on a bounded event. Model conclusions remain labeled inference.
// Each input is a prevalidated finite batch; cleanup uses an independent context.
func Run(ctx context.Context, p Policy, env Environment, controller Controller, journal io.Writer) (result Result, err error) {
	if err = p.Validate(); err != nil {
		return result, err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.BudgetMS)*time.Millisecond)
	defer cancel()
	enc := json.NewEncoder(journal)
	emit := func(kind string, value any) error {
		return enc.Encode(map[string]any{"kind": kind, "elapsed_ms": time.Since(start).Milliseconds(), "data": value})
	}
	defer func() {
		cleanCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		cleanup, errCleanup := env.Cancel(cleanCtx)
		result.Cleanup = cleanup
		if errCleanup != nil {
			result.CleanupError = errCleanup.Error()
		}
		result.ElapsedMS = time.Since(start).Milliseconds()
		if e := emit("handback", result); e != nil && err == nil {
			err = e
		}
	}()
	if err = emit("policy", p); err != nil {
		return result, err
	}
	var previous *Observation
	last := ""
	stall := 0
	var priorProgress string
	for {
		if ctx.Err() != nil {
			result.Reason = "deadline_or_cancelled"
			return result, nil
		}
		obs, e := env.Observe(ctx)
		if e != nil {
			result.Reason = "observation_failed"
			result.Error = e.Error()
			return result, nil
		}
		result.Current = &obs
		if err = emit("observation", obs); err != nil {
			return result, err
		}
		document := observationDocument(obs)
		progress := make([]any, 0, len(p.ProgressPointers))
		for _, ptr := range p.ProgressPointers {
			v, ok := pointer(document, ptr)
			if !ok {
				result.Reason = "missing_progress_fact"
				result.Error = ptr
				return result, nil
			}
			progress = append(progress, v)
		}
		encoded, _ := json.Marshal(progress)
		currentProgress := string(encoded)
		if previous != nil {
			if currentProgress == priorProgress {
				stall++
			} else {
				stall = 0
			}
		}
		priorProgress = currentProgress
		for _, condition := range p.StopWhen {
			v, ok := pointer(document, condition.Pointer)
			if !ok {
				result.Reason = "missing_threshold_fact"
				result.Error = condition.Pointer
				return result, nil
			}
			if compare(v, condition.Operator, condition.Value) {
				result.Reason = condition.Reason
				return result, nil
			}
		}
		if stall >= p.StallActions {
			result.Reason = "stalled_observed_facts"
			return result, nil
		}
		if result.Actions >= p.MaxActions {
			result.Reason = "action_limit"
			return result, nil
		}
		deadline, _ := ctx.Deadline()
		state := State{p.Goal, p.Instructions, obs, previous, last, result.Actions, time.Until(deadline).Milliseconds()}
		decisionCtx, done := context.WithTimeout(ctx, time.Duration(p.DecisionTimeoutMS)*time.Millisecond)
		decisionStart := time.Now()
		decision, e := controller.Decide(decisionCtx, state, p.Tactics)
		done()
		if err = emit("controller_inference", map[string]any{"decision": decision, "latency_ms": time.Since(decisionStart).Milliseconds()}); err != nil {
			return result, err
		}
		if e != nil {
			result.Reason = "controller_failed"
			result.Error = e.Error()
			return result, nil
		}
		if math.IsNaN(decision.Confidence) || decision.Confidence < p.MinimumConfidence || decision.Confidence > 1 {
			result.Reason = "low_confidence"
			return result, nil
		}
		switch decision.Choice {
		case "goal_complete", "stalled", "unexpected_state", "uncertain":
			result.Reason = "controller_" + decision.Choice
			return result, nil
		}
		var chosen *Tactic
		for i := range p.Tactics {
			if p.Tactics[i].ID == decision.Choice {
				chosen = &p.Tactics[i]
				break
			}
		}
		if chosen == nil {
			result.Reason = "invalid_decision"
			return result, nil
		}
		if ctx.Err() != nil {
			result.Reason = "deadline_or_cancelled"
			return result, nil
		}
		if err = emit("action_requested", chosen); err != nil {
			return result, err
		}
		receipt, e := env.Input(ctx, chosen.Steps)
		if err = emit("action_receipt", receipt); err != nil {
			return result, err
		}
		if e != nil {
			result.Reason = "input_delivery_uncertain"
			result.Error = e.Error()
			return result, nil
		}
		result.Actions++
		last = chosen.ID
		copyObs := obs
		previous = &copyObs
	}
}
func observationDocument(o Observation) any {
	b, _ := json.Marshal(o)
	var v any
	json.Unmarshal(b, &v)
	return v
}
func pointer(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	for _, key := range strings.Split(path[1:], "/") {
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[key]
			if !ok {
				return nil, false
			}
		case []any:
			i, e := strconv.Atoi(key)
			if e != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
func compare(a any, op string, b any) bool {
	if op == "eq" {
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		return string(aa) == string(bb)
	}
	x, ok := a.(float64)
	if !ok {
		return false
	}
	y, ok := b.(float64)
	if !ok {
		return false
	}
	if op == "lte" {
		return x <= y
	}
	return x >= y
}

type Baseline struct{}

func (b *Baseline) Decide(_ context.Context, _ State, t []Tactic) (Decision, error) {
	choice := t[0].ID
	return Decision{Choice: choice, Confidence: 1}, nil
}
