package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// Jev uses OpenRouter's typed decisions API through den-router. It never asks
// for generated code, prose, or arbitrary input instructions.
type Jev struct {
	View    *DecisionView
	Groups  []ControlGroup
	BaseURL string
	Model   string
	Token   string
	HTTP    *http.Client
}

func (j *Jev) Decide(ctx context.Context, state State, tactics []Tactic) (Decision, error) {
	criteria := map[string]string{
		"goal_complete":    "The available observed facts establish the planner's goal is complete.",
		"stalled":          "The available observations show no useful progress and require planner intervention.",
		"unexpected_state": "The observations show an unexpected condition outside the permitted tactics.",
		"uncertain":        "The available facts are insufficient to choose a useful permitted action reliably.",
	}
	for _, t := range tactics {
		criteria[t.ID] = t.Description
	}
	// Screenshot paths are evidence references, not visual content that Jev can see.
	payload := map[string]any{"model": j.Model, "state": j.View.State(state), "questions": map[string]any{"next_action": map[string]any{"type": "choice", "instructions": "Choose the next short playtesting tactic using the goal, policy, current observed facts and previous observation. Follow the active parent_guidance objective, target, parameters and tactical priorities when present; it refines but cannot override the fixed mission or hard limits. Use fresh facts to correct actions as the situation changes. Select a handback choice when appropriate. Screenshot paths do not reveal their image contents. Do not infer hidden events or assume a chosen action succeeded. Respect axes and facing in product facts.", "criteria": criteria}}}
	if len(j.Groups) > 0 {
		if err := validateGroups(j.Groups, tactics); err != nil {
			return Decision{}, err
		}
		questions := map[string]any{"status": map[string]any{"type": "choice", "instructions": "Continue ordinary gameplay while alive and the time budget remains. Hand back only if no supplied control can address the observed situation; blocked movement or an occluded enemy normally needs repositioning, not handback.", "criteria": map[string]string{"continue": "Continue controlling the game", "goal_complete": criteria["goal_complete"], "stalled": criteria["stalled"], "unexpected_state": criteria["unexpected_state"], "uncertain": criteria["uncertain"]}}}
		for _, g := range j.Groups {
			options := map[string]string{}
			for _, id := range g.Tactics {
				options[id] = criteria[id]
			}
			questions[g.ID] = map[string]any{"type": "choice", "instructions": g.Instructions + " Choose using current facts and observed changes; stale parent numerical angles are not current measurements. This field acts simultaneously with the other control groups.", "criteria": options}
		}
		payload["questions"] = questions
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(j.BaseURL, "/")+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if j.Token != "" {
		req.Header.Set("Authorization", "Bearer "+j.Token)
	}
	transport := j.HTTP
	if transport == nil {
		transport = http.DefaultClient
	}
	resp, err := transport.Do(req)
	if err != nil {
		return Decision{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return Decision{}, err
	}
	if len(raw) > 1<<20 {
		return Decision{}, errors.New("decision response exceeded 1MiB")
	}
	if resp.StatusCode != 200 {
		return Decision{}, fmt.Errorf("decision endpoint HTTP %d: %.500s", resp.StatusCode, raw)
	}
	var response struct {
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return Decision{}, err
	}
	field := "next_action"
	if len(j.Groups) > 0 {
		field = "status"
	}
	answer, ok := response.Answers[field]
	if !ok || answer.Type != "choice" || answer.Confidence == nil {
		return Decision{}, fmt.Errorf("missing typed %s choice/confidence", field)
	}
	if math.IsNaN(*answer.Confidence) || *answer.Confidence < 0 || *answer.Confidence > 1 {
		return Decision{}, fmt.Errorf("invalid %s confidence", field)
	}
	d := Decision{RequestBytes: len(body), Choice: answer.Choice, Confidence: *answer.Confidence, Raw: raw, Probabilities: map[string]map[string]float64{field: answer.Probabilities}}
	if len(j.Groups) > 0 && answer.Choice == "continue" {
		d.Choices = map[string]string{}
		for _, g := range j.Groups {
			a, ok := response.Answers[g.ID]
			if !ok || a.Type != "choice" || a.Confidence == nil {
				return Decision{}, fmt.Errorf("missing typed %s", g.ID)
			}
			if math.IsNaN(*a.Confidence) || *a.Confidence < 0 || *a.Confidence > 1 {
				return Decision{}, errors.New("invalid control confidence")
			}
			if *a.Confidence < d.Confidence {
				d.Confidence = *a.Confidence
			}
			d.Choices[g.ID] = a.Choice
			d.Probabilities[g.ID] = a.Probabilities
		}
	}
	return d, nil
}
