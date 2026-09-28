package assistant

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
)

type HTTPParent struct {
	BaseURL, Model, Protocol, Token string
	HTTP                            *http.Client
	Vision                          bool
}

const parentInstructions = `You are the strategic/tactical supervisor of a fast Jev game controller. Jev keeps acting while you think. Use the current product-authored facts, previous observation, current guidance and finite tactic menu to revise the situation framing and control objectives. Do not issue a step-by-step input script or product command. Give compact goal-seeking parameters and useful priorities (engagement distance, retreat criteria, aim/fire guidance). If navigation.targets is supplied and a destination change is needed, set navigation_target to exactly one supplied target ID; otherwise omit it or leave it empty. Never invent a target ID, route, waypoint, bearing, or distance. The fixed mission and hard limits remain authoritative. Observations may have changed when this advice arrives: prefer stable guidance, not a momentary keypress. Distinguish unknown from known. Return ONLY one JSON object with fields situation:string, objective:string, navigation_target:string, parameters:object, preferred_tactics:string[], stop:boolean. Preferred tactics must name supplied IDs. Stop only when the overall mission is achieved or further action needs the supervising agent. Keep the response compact, under 120 words, with at most five parameters.`

const parentTextOnlyInstructions = ` No image is attached. Screenshot metadata, when present in evidence, is not visual observation; do not claim to have seen its pixels.`

const parentVisionInstructions = ` A current PNG from this observation is attached as image evidence. You have seen those actual pixels, but they describe only the capture timestamp and can become stale while you think. Reconcile the image with the supplied fresh product facts. State stable maneuver intents, completion conditions, and recovery or handback criteria using fresh facts; never prescribe a stale key sequence.`

const maxParentImageBytes = 16 << 20

func (p *HTTPParent) Advise(ctx context.Context, state State, tactics []Tactic) (ParentDecision, error) {
	menu := make(map[string]string, len(tactics))
	for _, t := range tactics {
		menu[t.ID] = t.Description
	}
	input, err := json.Marshal(map[string]any{"state": semanticState(state), "available_tactics": menu})
	if err != nil {
		return ParentDecision{}, err
	}
	var image *parentRequestImage
	var imageEvidence *ParentImageEvidence
	if p.Vision {
		image, imageEvidence, err = parentImage(state.Observation)
		if err != nil {
			return ParentDecision{}, err
		}
	}
	instructions := parentInstructions
	if p.Vision {
		instructions += parentVisionInstructions
	} else {
		instructions += parentTextOnlyInstructions
	}
	protocol := p.Protocol
	if protocol == "" {
		protocol = "chat"
	}
	endpoint := "/v1/chat/completions"
	var payload map[string]any
	switch protocol {
	case "chat":
		if image == nil {
			payload = map[string]any{"model": p.Model, "messages": []map[string]string{{"role": "system", "content": instructions}, {"role": "user", "content": string(input)}}, "max_tokens": 1200, "reasoning_effort": "low", "stream": false}
			break
		}
		content := []any{map[string]string{"type": "text", "text": string(input)}}
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": image.DataURL}})
		payload = map[string]any{"model": p.Model, "messages": []any{map[string]any{"role": "system", "content": instructions}, map[string]any{"role": "user", "content": content}}, "max_tokens": 1200, "reasoning_effort": "low", "stream": false}
	case "responses":
		endpoint = "/v1/responses"
		content := []any{map[string]string{"type": "input_text", "text": string(input)}}
		if image != nil {
			content = append(content, map[string]string{"type": "input_image", "image_url": image.DataURL})
		}
		payload = map[string]any{"model": p.Model, "instructions": instructions, "input": []any{map[string]any{"role": "user", "content": content}}, "reasoning": map[string]string{"effort": "low"}, "stream": true, "store": false}
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
		return ParentDecision{InputImage: imageEvidence}, fmt.Errorf("parent HTTP %d: %s", resp.StatusCode, raw)
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
				return ParentDecision{InputImage: imageEvidence}, fmt.Errorf("parent stream %s", event.Type)
			}
		}
		if err = scanner.Err(); err != nil {
			return ParentDecision{InputImage: imageEvidence}, err
		}
		if !completed {
			return ParentDecision{InputImage: imageEvidence}, errors.New("parent response stream ended without completion")
		}
		text = out.String()
	} else {
		raw, err = io.ReadAll(io.LimitReader(reader, (2<<20)+1))
		if err != nil {
			return ParentDecision{InputImage: imageEvidence}, err
		}
		if len(raw) > 2<<20 {
			return ParentDecision{InputImage: imageEvidence}, errors.New("parent response exceeds2MiB")
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
			return ParentDecision{InputImage: imageEvidence}, err
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
		return ParentDecision{Raw: raw, InputImage: imageEvidence}, fmt.Errorf("parent did not return guidance JSON: %w", err)
	}
	if err = guidance.validate(tactics); err != nil {
		return ParentDecision{Raw: raw, InputImage: imageEvidence}, err
	}
	return ParentDecision{Guidance: guidance, Raw: raw, InputImage: imageEvidence}, nil
}

type parentRequestImage struct{ DataURL string }

func parentImage(observation Observation) (*parentRequestImage, *ParentImageEvidence, error) {
	var capture struct {
		Path       string `json:"path"`
		CaptureID  string `json:"capture_id"`
		ArtifactID string `json:"artifact_id"`
		Status     string `json:"status"`
	}
	if len(observation.Capture) == 0 || json.Unmarshal(observation.Capture, &capture) != nil || strings.TrimSpace(capture.Path) == "" {
		if capture.Status != "" {
			return nil, nil, fmt.Errorf("parent vision requires a current capture with a local PNG path (capture status %q)", capture.Status)
		}
		return nil, nil, errors.New("parent vision requires a current capture with a local PNG path")
	}
	info, err := os.Stat(capture.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read parent vision capture %q: %w", capture.Path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("parent vision capture %q is not a regular file", capture.Path)
	}
	if info.Size() > maxParentImageBytes {
		return nil, nil, fmt.Errorf("parent vision capture %q exceeds %d bytes", capture.Path, maxParentImageBytes)
	}
	pixels, err := os.ReadFile(capture.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read parent vision capture %q: %w", capture.Path, err)
	}
	if len(pixels) == 0 {
		return nil, nil, fmt.Errorf("parent vision capture %q is empty", capture.Path)
	}
	config, err := png.DecodeConfig(bytes.NewReader(pixels))
	if err != nil || config.Width < 1 || config.Height < 1 {
		return nil, nil, fmt.Errorf("parent vision capture %q is not a valid PNG", capture.Path)
	}
	digest := sha256.Sum256(pixels)
	captureID := capture.CaptureID
	if captureID == "" {
		captureID = capture.ArtifactID
	}
	evidence := &ParentImageEvidence{Path: capture.Path, SHA256: fmt.Sprintf("%x", digest), Bytes: len(pixels), ObservedAt: observation.CapturedAt, CaptureID: captureID}
	return &parentRequestImage{DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(pixels)}, evidence, nil
}

// Keep capture metadata out of model context. The opt-in visual path attaches
// only the current image; previous facts supply change context cheaply.
func semanticState(s State) State {
	s.Observation.Capture = nil
	if s.Previous != nil {
		previous := *s.Previous
		previous.Capture = nil
		s.Previous = &previous
	}
	return s
}
