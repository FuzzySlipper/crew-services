package dshdriver

import (
	"encoding/json"
	"testing"
)

func TestEventModelReadsAssistantMessages(t *testing.T) {
	assistant := Event{Type: "assistant/message", Data: json.RawMessage(`{"message":{"role":"assistant","source":{"model":"deepseek-flash"}}}`)}
	policy := Event{Type: "subagent/model-selection-policy", Data: json.RawMessage(`{"allowedModels":[{"model":"glm-5.3-flash"}]}`)}
	if got := eventModel(assistant); got != "deepseek-flash" {
		t.Fatalf("assistant = %q", got)
	}
	if got := eventModel(policy); got != "" {
		t.Fatalf("a model listing was taken as the model: %q", got)
	}
}
