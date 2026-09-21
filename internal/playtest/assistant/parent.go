package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Guidance adjusts semantic objectives and bounds; it cannot synthesize controls
// or change the caller's hard deadlines, health thresholds, or tactic menu.
type Guidance struct {
	Situation        string         `json:"situation"`
	Objective        string         `json:"objective"`
	Target           string         `json:"target"`
	Parameters       map[string]any `json:"parameters"`
	PreferredTactics []string       `json:"preferred_tactics"`
	Stop             bool           `json:"stop"`
}
type AppliedGuidance struct {
	Guidance
	Revision         int       `json:"revision"`
	BasedOnAction    int       `json:"based_on_action"`
	AppliedAtAction  int       `json:"applied_at_action"`
	SourceObservedAt time.Time `json:"source_observed_at"`
}
type ParentPolicy struct {
	EveryActions  int      `json:"every_actions"`
	TimeoutMS     int      `json:"timeout_ms"`
	MaxAgeMS      int      `json:"max_age_ms"`
	EventPointers []string `json:"event_pointers,omitempty"`
}
type ParentDecision struct {
	Guidance Guidance        `json:"guidance"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}
type Parent interface {
	Advise(context.Context, State, []Tactic) (ParentDecision, error)
}
type parentReply struct {
	Decision         ParentDecision `json:"decision"`
	Started          time.Time      `json:"started_at"`
	Finished         time.Time      `json:"finished_at"`
	BasedOnAction    int            `json:"based_on_action"`
	SourceObservedAt time.Time      `json:"source_observed_at"`
	Error            string         `json:"error,omitempty"`
}

func (p *ParentPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.EveryActions < 1 || p.EveryActions > 50 || p.TimeoutMS < 100 || p.TimeoutMS > 60000 || p.MaxAgeMS < 100 || p.MaxAgeMS > 60000 {
		return errors.New("parent requires every_actions1..50, timeout_ms/max_age_ms100..60000")
	}
	return nil
}
func (g Guidance) validate(tactics []Tactic) error {
	if g.Objective == "" && !g.Stop {
		return errors.New("parent guidance needs objective or stop")
	}
	ids := map[string]bool{}
	for _, t := range tactics {
		ids[t.ID] = true
	}
	for _, id := range g.PreferredTactics {
		if !ids[id] {
			return errors.New("parent preferred an unavailable tactic")
		}
	}
	return nil
}
