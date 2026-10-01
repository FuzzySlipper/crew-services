package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAssistResultsKeepAFullReceipt(t *testing.T) {
	s := &Service{stateDir: t.TempDir()}
	result := map[string]any{"accepted": true, "observation": map[string]any{"player": map[string]any{"health": 100}}}
	s.keepReceipt("s1", json.RawMessage(`{"op":"playtest","request":{"op":"act","id":"forward"}}`), result)
	path, _ := result["receipt"].(string)
	if !strings.Contains(path, "-act-") {
		t.Fatalf("receipt path: %v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"health": 100`) || !strings.Contains(string(data), `"id": "forward"`) {
		t.Fatalf("receipt content: %s %v", data, err)
	}
	other := map[string]any{"clicked": true}
	s.keepReceipt("s1", json.RawMessage(`{"op":"click","selector":"#go"}`), other)
	if _, ok := other["receipt"]; ok {
		t.Fatal("a DOM operation got a receipt")
	}
}

func TestEngineScriptsMayMakeMoreCalls(t *testing.T) {
	if scriptCallLimit(&Session{Profile: &Profile{Backend: "engine"}}) != 4096 || scriptCallLimit(&Session{Profile: &Profile{}}) != 512 || scriptCallLimit(nil) != 512 {
		t.Fatal("call limits")
	}
}
