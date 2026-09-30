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

	"crew-services/internal/playtest/input"
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
	ControlGroups     []ControlGroup `json:"control_groups,omitempty"`
	DecisionMaxAgeMS  int            `json:"decision_max_age_ms,omitempty"`
	Goal              string         `json:"goal"`
	Instructions      string         `json:"instructions"`
	BudgetMS          int            `json:"budget_ms"`
	MaxActions        int            `json:"max_actions"`
	DecisionTimeoutMS int            `json:"decision_timeout_ms"`
	MinimumConfidence float64        `json:"minimum_confidence"`
	StallActions      int            `json:"stall_actions"`
	ProgressPointers  []string       `json:"progress_pointers"`
	StopWhen          []Condition    `json:"stop_when"`
	Tactics           []Tactic       `json:"tactics"`
	Parent            *ParentPolicy  `json:"parent,omitempty"`
}
type Decision struct {
	RequestBytes  int                           `json:"request_bytes,omitempty"`
	Choices       map[string]string             `json:"choices,omitempty"`
	Probabilities map[string]map[string]float64 `json:"probabilities,omitempty"`
	Choice        string                        `json:"choice"`
	Confidence    float64                       `json:"confidence"`
	Raw           json.RawMessage               `json:"raw,omitempty"`
}
type State struct {
	Goal          string           `json:"goal"`
	Instructions  string           `json:"instructions"`
	Observation   Observation      `json:"observation"`
	Previous      *Observation     `json:"previous,omitempty"`
	LastAction    string           `json:"last_action,omitempty"`
	ActionsTaken  int              `json:"actions_taken"`
	RemainingMS   int64            `json:"remaining_ms"`
	Guidance      *AppliedGuidance `json:"parent_guidance,omitempty"`
	RecentActions []string         `json:"recent_actions,omitempty"`
}
type Controller interface {
	Decide(context.Context, State, []Tactic) (Decision, error)
}
type Environment interface {
	Observe(context.Context) (Observation, error)
	Input(context.Context, []map[string]any) (json.RawMessage, error)
	Cancel(context.Context) (json.RawMessage, error)
}
type navigationEnvironment interface {
	SetNavigationTarget(string) error
	NavigationEnabled() bool
}
type Result struct {
	Reason        string          `json:"reason"`
	Error         string          `json:"error,omitempty"`
	Actions       int             `json:"actions"`
	ElapsedMS     int64           `json:"elapsed_ms"`
	Current       *Observation    `json:"current,omitempty"`
	Cleanup       json.RawMessage `json:"cleanup,omitempty"`
	CleanupError  string          `json:"cleanup_error,omitempty"`
	ParentPending bool            `json:"parent_pending_on_handback,omitempty"`
}

func (p Policy) Validate() error {
	if p.DecisionMaxAgeMS < 0 || p.DecisionMaxAgeMS > 30000 {
		return errors.New("decision_max_age_ms must be 0(disabled)..30000")
	}
	if err := p.Parent.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(p.Goal) == "" || p.BudgetMS < 100 || p.BudgetMS > 120000 || p.MaxActions < 0 || p.MaxActions > 100 || p.DecisionTimeoutMS < 100 || p.DecisionTimeoutMS > 30000 {
		return errors.New("goal and bounded budget_ms(100..120000), max_actions(0=uncapped,1..100), decision_timeout_ms(100..30000) required")
	}
	if math.IsNaN(p.MinimumConfidence) || p.MinimumConfidence < 0 || p.MinimumConfidence > 1 {
		return errors.New("minimum_confidence must be 0..1")
	}
	if p.StallActions < 0 || len(p.ProgressPointers) == 0 {
		return errors.New("nonnegative stall_actions (0=disabled) and progress_pointers required")
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
		if err := input.ValidateBatch(t.Steps); err != nil {
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
	if err := validateGroups(p.ControlGroups, p.Tactics); err != nil {
		return err
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
func Run(ctx context.Context, p Policy, env Environment, controller Controller, journal io.Writer) (Result, error) {
	return RunWithParent(ctx, p, env, controller, nil, journal)
}

func RunWithParent(ctx context.Context, p Policy, env Environment, controller Controller, parent Parent, journal io.Writer) (result Result, err error) {
	if err = p.Validate(); err != nil {
		return result, err
	}
	if (p.Parent == nil) != (parent == nil) {
		return result, errors.New("parent policy and parent client must be supplied together")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.BudgetMS)*time.Millisecond)
	defer cancel()
	parentBusy := false
	enc := json.NewEncoder(journal)
	emit := func(kind string, value any) error {
		return enc.Encode(map[string]any{"kind": kind, "elapsed_ms": time.Since(start).Milliseconds(), "data": value})
	}
	defer func() {
		result.ParentPending = parentBusy
		cancel()
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
	replies := make(chan parentReply, 1)
	lastParentAction := -1
	var lastParentEvents string
	var guidance *AppliedGuidance
	var pendingNavigationGuidance *AppliedGuidance
	var lastInputEnd time.Time
	var recentActions []string
	navigation, navigationConfigured := env.(navigationEnvironment)
	if navigationConfigured && !navigation.NavigationEnabled() {
		navigationConfigured = false
	}
	for {
		if ctx.Err() != nil {
			result.Reason = "deadline_or_cancelled"
			return result, nil
		}
		observationStart := time.Now()
		obs, e := env.Observe(ctx)
		if e != nil {
			result.Reason = "observation_failed"
			if ctx.Err() != nil {
				result.Reason = "deadline_or_cancelled"
			}
			result.Error = e.Error()
			return result, nil
		}
		observationDuration := time.Since(observationStart).Milliseconds()
		result.Current = &obs
		if err = emit("observation", obs); err != nil {
			return result, err
		}
		if pendingNavigationGuidance != nil {
			if obs.NavigationTarget != pendingNavigationGuidance.NavigationTarget {
				result.Reason = "navigation_observation_mismatch"
				result.Error = "fresh route observation did not match the pending navigation target"
				return result, nil
			}
			guidance = pendingNavigationGuidance
			pendingNavigationGuidance = nil
			if err = emit("navigation_guidance_adopted", map[string]any{"guidance_revision": guidance.Revision, "target": guidance.NavigationTarget, "route_fact": "navigation.route", "source_observed_at": obs.CapturedAt}); err != nil {
				return result, err
			}
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
		if p.StallActions > 0 && stall >= p.StallActions {
			result.Reason = "stalled_observed_facts"
			return result, nil
		}
		if p.MaxActions > 0 && result.Actions >= p.MaxActions {
			result.Reason = "action_limit"
			return result, nil
		}
		deadline, _ := ctx.Deadline()
		state := State{Goal: p.Goal, Instructions: p.Instructions, Observation: obs, Previous: previous, LastAction: last, ActionsTaken: result.Actions, RemainingMS: time.Until(deadline).Milliseconds(), Guidance: guidance, RecentActions: append([]string(nil), recentActions...)}
		// Parent inference runs concurrently. Only this loop writes the journal and
		// installs a completed update, at an action boundary.
		if parent != nil {
			observeNewNavigationTarget := false
			select {
			case reply := <-replies:
				parentBusy = false
				disposition := "applied"
				if reply.Error != "" {
					disposition = "failed"
				} else if time.Since(reply.SourceObservedAt) > time.Duration(p.Parent.MaxAgeMS)*time.Millisecond {
					disposition = "stale"
				} else if e := reply.Decision.Guidance.validate(p.Tactics); e != nil {
					reply.Error = e.Error()
					disposition = "invalid"
				}
				if disposition == "applied" {
					revision := 1
					if guidance != nil {
						revision = guidance.Revision + 1
					}
					candidate := reply.Decision.Guidance
					if candidate.Stop {
						// A valid handback is immediate. It neither adopts a requested
						// destination nor asks the product for another route.
						if navigationConfigured {
							candidate.NavigationTarget = obs.NavigationTarget
						} else if candidate.NavigationTarget == "" && guidance != nil {
							candidate.NavigationTarget = guidance.NavigationTarget
						}
						guidance = &AppliedGuidance{Guidance: candidate, Revision: revision, BasedOnAction: reply.BasedOnAction, AppliedAtAction: result.Actions, SourceObservedAt: reply.SourceObservedAt}
						state.Guidance = guidance
					} else if candidate.NavigationTarget != "" {
						if !validNavigationTarget(candidate.NavigationTarget) {
							reply.Error = "invalid navigation_target suggestion"
							disposition = "invalid_navigation_target"
						} else if ids, available, targetErr := observationNavigationTargets(obs); targetErr != nil {
							reply.Error = targetErr.Error()
							disposition = "invalid_navigation_target"
						} else if !available {
							reply.Error = "navigation.targets was not observed"
							disposition = "invalid_navigation_target"
						} else if _, exists := ids[candidate.NavigationTarget]; !exists {
							reply.Error = "navigation_target is absent from latest navigation.targets"
							disposition = "invalid_navigation_target"
						}
					} else if navigationConfigured {
						// Empty parent target means retain the accepted product target.
						candidate.NavigationTarget = obs.NavigationTarget
					} else if guidance != nil {
						// Without route assistance, an omitted parent target retains its
						// prior strategic destination and causes no product query.
						candidate.NavigationTarget = guidance.NavigationTarget
					}
					if disposition == "applied" {
						applied := &AppliedGuidance{Guidance: candidate, Revision: revision, BasedOnAction: reply.BasedOnAction, AppliedAtAction: result.Actions, SourceObservedAt: reply.SourceObservedAt}
						if navigationConfigured && applied.NavigationTarget != obs.NavigationTarget {
							if targetErr := navigation.SetNavigationTarget(applied.NavigationTarget); targetErr != nil {
								reply.Error = targetErr.Error()
								disposition = "invalid_navigation_target"
							} else {
								pendingNavigationGuidance = applied
								disposition = "deferred_for_navigation_observation"
								observeNewNavigationTarget = true
							}
						} else {
							guidance = applied
							state.Guidance = guidance
						}
					}
				}
				if err = emit("parent_result", map[string]any{"reply": reply, "disposition": disposition, "applied_at_action": result.Actions, "actions_while_pending": result.Actions - reply.BasedOnAction, "latency_ms": reply.Finished.Sub(reply.Started).Milliseconds(), "active_navigation_target": obs.NavigationTarget, "guidance_navigation_target": reply.Decision.Guidance.NavigationTarget}); err != nil {
					return result, err
				}
				if disposition == "applied" && guidance.Stop {
					result.Reason = "parent_requested_handback"
					return result, nil
				}
			default:
			}
			if observeNewNavigationTarget {
				// Do not give the controller new destination guidance beside an old route.
				continue
			}
			events := make([]any, 0, len(p.Parent.EventPointers))
			for _, ptr := range p.Parent.EventPointers {
				v, ok := pointer(document, ptr)
				if !ok {
					result.Reason = "missing_parent_event_fact"
					result.Error = ptr
					return result, nil
				}
				events = append(events, v)
			}
			rawEvents, _ := json.Marshal(events)
			eventKey := string(rawEvents)
			due := lastParentAction < 0 || result.Actions-lastParentAction >= p.Parent.EveryActions || eventKey != lastParentEvents
			if !parentBusy && due {
				parentBusy = true
				lastParentAction = result.Actions
				lastParentEvents = eventKey
				revision := 0
				if guidance != nil {
					revision = guidance.Revision
				}
				if err = emit("parent_requested", map[string]any{"based_on_action": result.Actions, "guidance_revision": revision, "source_observed_at": obs.CapturedAt}); err != nil {
					return result, err
				}
				go func(snapshot State) {
					began := time.Now()
					requestCtx, done := context.WithTimeout(ctx, time.Duration(p.Parent.TimeoutMS)*time.Millisecond)
					defer done()
					decision, e := parent.Advise(requestCtx, snapshot, p.Tactics)
					reply := parentReply{Decision: decision, Started: began, Finished: time.Now(), BasedOnAction: snapshot.ActionsTaken, SourceObservedAt: snapshot.Observation.CapturedAt}
					if e != nil {
						reply.Error = e.Error()
					}
					select {
					case replies <- reply:
					case <-ctx.Done():
					}
				}(state)
			}
		}
		decisionCtx, done := context.WithTimeout(ctx, time.Duration(p.DecisionTimeoutMS)*time.Millisecond)
		decisionStart := time.Now()
		decision, e := controller.Decide(decisionCtx, state, p.Tactics)
		done()
		if err = emit("controller_inference", map[string]any{"decision": decision, "latency_ms": time.Since(decisionStart).Milliseconds()}); err != nil {
			return result, err
		}
		if ctx.Err() != nil {
			result.Reason = "deadline_or_cancelled"
			return result, nil
		}
		if e != nil {
			result.Reason = "controller_failed"
			result.Error = e.Error()
			return result, nil
		}
		if p.DecisionMaxAgeMS > 0 && time.Since(obs.CapturedAt) > time.Duration(p.DecisionMaxAgeMS)*time.Millisecond {
			if err = emit("decision_discarded", map[string]any{"reason": "stale_observation", "age_ms": time.Since(obs.CapturedAt).Milliseconds()}); err != nil {
				return result, err
			}
			continue
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
		if len(p.ControlGroups) > 0 {
			chosen, e = composeControls(p.ControlGroups, p.Tactics, decision.Choices)
			if e != nil {
				result.Reason = "invalid_decision"
				result.Error = e.Error()
				return result, nil
			}
		} else {
			for i := range p.Tactics {
				if p.Tactics[i].ID == decision.Choice {
					chosen = &p.Tactics[i]
					break
				}
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
		// Do not start a known finite hold that cannot finish before the budget.
		// Reserve transport headroom; truly uncertain delivery still stays uncertain.
		rawSteps, _ := json.Marshal(chosen.Steps)
		var holds []struct {
			MS int `json:"ms"`
		}
		json.Unmarshal(rawSteps, &holds)
		holdMS := 0
		for _, h := range holds {
			holdMS += h.MS
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= time.Duration(holdMS+100)*time.Millisecond {
			if err = emit("action_skipped_budget", map[string]any{"hold_ms": holdMS}); err != nil {
				return result, err
			}
			<-ctx.Done()
			result.Reason = "deadline_or_cancelled"
			return result, nil
		}
		if err = emit("action_requested", chosen); err != nil {
			return result, err
		}
		inputStart := time.Now()
		gapMS := int64(0)
		if !lastInputEnd.IsZero() {
			gapMS = inputStart.Sub(lastInputEnd).Milliseconds()
		}
		receipt, e := env.Input(ctx, chosen.Steps)
		lastInputEnd = time.Now()
		revision := 0
		if guidance != nil {
			revision = guidance.Revision
		}
		if timingErr := emit("cycle_timing", map[string]any{"action": chosen.ID, "action_index": result.Actions, "observation_ms": observationDuration, "observation_age_at_input_ms": inputStart.Sub(obs.CapturedAt).Milliseconds(), "input_call_ms": lastInputEnd.Sub(inputStart).Milliseconds(), "input_gap_ms": gapMS, "guidance_revision": revision, "parent_pending": parentBusy}); timingErr != nil {
			return result, timingErr
		}
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
		recentActions = append(recentActions, chosen.ID)
		if len(recentActions) > 15 {
			recentActions = recentActions[len(recentActions)-15:]
		}
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
