package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Jev uses OpenRouter's typed decisions API through den-router. It never asks
// for generated code, prose, or arbitrary input instructions.
type Jev struct {
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
	payload := map[string]any{"model": j.Model, "state": state, "questions": map[string]any{"next_action": map[string]any{"type": "choice", "instructions": "Choose the next short playtesting tactic using the goal, policy, current observed facts and previous observation. Select a handback choice when appropriate. Screenshot paths do not reveal their image contents. Do not infer hidden events or assume a chosen action succeeded. Respect axes and facing in product facts.", "criteria": criteria}}}
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
			Type       string   `json:"type"`
			Choice     string   `json:"choice"`
			Confidence *float64 `json:"confidence"`
		} `json:"answers"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return Decision{}, err
	}
	answer, ok := response.Answers["next_action"]
	if !ok || answer.Type != "choice" || answer.Confidence == nil {
		return Decision{}, errors.New("missing typed next_action choice/confidence")
	}
	return Decision{Choice: answer.Choice, Confidence: *answer.Confidence, Raw: raw}, nil
}
