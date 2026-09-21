package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type HTTPParent struct {
	BaseURL, Model, Protocol, Token string
	HTTP                            *http.Client
}

const parentInstructions = `You are the strategic/tactical supervisor of a fast Jev game controller. Jev keeps acting while you think. Use the current product-authored facts, previous observation, current guidance and finite tactic menu to revise the situation framing and control objectives. Do not issue a step-by-step input script. Give compact goal-seeking parameters and useful priorities (target, route or destination, engagement distance, retreat criteria, aim/fire guidance). The fixed mission and hard limits remain authoritative. Observations may have changed when this advice arrives: prefer stable guidance, not a momentary keypress. Screenshot paths are evidence references only; you have not seen their pixels. Distinguish unknown from known. Return ONLY one JSON object with fields situation:string, objective:string, target:string, parameters:object, preferred_tactics:string[], stop:boolean. Preferred tactics must name supplied IDs. Stop only when the overall mission is achieved or further action needs the supervising agent. Keep the response compact, under 120 words, with at most five parameters.`

func (p *HTTPParent) Advise(ctx context.Context, state State, tactics []Tactic) (ParentDecision, error) {
	menu := make(map[string]string, len(tactics))
	for _, t := range tactics {
		menu[t.ID] = t.Description
	}
	input, _ := json.Marshal(map[string]any{"state": semanticState(state), "available_tactics": menu})
	protocol := p.Protocol
	if protocol == "" {
		protocol = "chat"
	}
	endpoint := "/v1/chat/completions"
	var payload map[string]any
	switch protocol {
	case "chat":
		payload = map[string]any{"model": p.Model, "messages": []map[string]string{{"role": "system", "content": parentInstructions}, {"role": "user", "content": string(input)}}, "max_tokens": 1200, "reasoning_effort": "low", "stream": false}
	case "responses":
		endpoint = "/v1/responses"
		payload = map[string]any{"model": p.Model, "instructions": parentInstructions, "input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": string(input)}}}}, "reasoning": map[string]string{"effort": "low"}, "stream": true, "store": false}
	default:
		return ParentDecision{}, errors.New("parent_protocol must be chat or responses")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ParentDecision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(p.BaseURL, "/")+endpoint, bytes.NewReader(body))
	if err != nil {
		return ParentDecision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	transport := p.HTTP
	if transport == nil {
		transport = http.DefaultClient
	}
	resp, err := transport.Do(req)
	if err != nil {
		return ParentDecision{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return ParentDecision{}, fmt.Errorf("parent HTTP %d: %s", resp.StatusCode, raw)
	}
	var text string
	var raw json.RawMessage
	reader := bufio.NewReader(resp.Body)
	prefix, _ := reader.Peek(6)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || string(prefix) == "event:" || string(prefix) == "data: " {
		scanner := bufio.NewScanner(io.LimitReader(reader, 2<<20))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		var out strings.Builder
		completed := false
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var event struct {
				Type     string          `json:"type"`
				Delta    string          `json:"delta"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(data), &event) != nil {
				continue
			}
			switch event.Type {
			case "response.output_text.delta":
				out.WriteString(event.Delta)
			case "response.completed":
				raw = event.Response
				completed = true
			case "response.failed", "response.incomplete", "error":
				return ParentDecision{}, fmt.Errorf("parent stream %s", event.Type)
			}
		}
		if err = scanner.Err(); err != nil {
			return ParentDecision{}, err
		}
		if !completed {
			return ParentDecision{}, errors.New("parent response stream ended without completion")
		}
		text = out.String()
	} else {
		raw, err = io.ReadAll(io.LimitReader(reader, (2<<20)+1))
		if err != nil {
			return ParentDecision{}, err
		}
		if len(raw) > 2<<20 {
			return ParentDecision{}, errors.New("parent response exceeds2MiB")
		}
		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return ParentDecision{}, err
		}
		if len(result.Choices) > 0 {
			text = result.Choices[0].Message.Content
		} else {
			for _, o := range result.Output {
				for _, c := range o.Content {
					text += c.Text
				}
			}
		}
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	var guidance Guidance
	if err = json.Unmarshal([]byte(text), &guidance); err != nil {
		return ParentDecision{Raw: raw}, fmt.Errorf("parent did not return guidance JSON: %w", err)
	}
	if err = guidance.validate(tactics); err != nil {
		return ParentDecision{Raw: raw}, err
	}
	return ParentDecision{Guidance: guidance, Raw: raw}, nil
}

// Keep evidence references in the transcript, not in model context. Pixels are
// never provided by this client. Previous facts supply change context cheaply.
func semanticState(s State) State {
	s.Observation.Capture = nil
	if s.Previous != nil {
		previous := *s.Previous
		previous.Capture = nil
		s.Previous = &previous
	}
	return s
}
