package codexadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCreatedControlStateReloadsOperationAndMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "crew-codex.json")
	want := persistedControlSession{Session: ControlSession{SessionID: "public-1", Label: "new", Status: "idle"}, Mapping: Mapping{Address: "codex/native-1", ThreadID: "native-1"}}
	if err := saveControlState(path, map[string]persistedControlSession{"click-1": want}); err != nil {
		t.Fatal(err)
	}
	controls := NewControls(nil, Config{StatePath: path})
	if got, err := controls.Create(t.Context(), "click-1", ""); err != nil || got != want.Session {
		t.Fatalf("replayed create = %#v, %v", got, err)
	}
	mappings := controls.Mappings(nil)
	if len(mappings) != 1 || mappings[0] != want.Mapping {
		t.Fatalf("reloaded mappings = %#v", mappings)
	}
}

func TestInteractionResponseShapesMatchSupportedCodexRequests(t *testing.T) {
	tests := []struct {
		method   string
		response string
		valid    bool
	}{
		{"item/commandExecution/requestApproval", `{"decision":"accept"}`, true},
		{"item/fileChange/requestApproval", `{"decision":"decline"}`, true},
		{"item/permissions/requestApproval", `{"permissions":{},"scope":"turn"}`, true},
		{"item/permissions/requestApproval", `{"permissions":{}}`, false},
		{"item/permissions/requestApproval", `{"decision":"accept"}`, false},
		{"item/tool/requestUserInput", `{"answers":{"choice":{"answers":["yes"]}}}`, true},
		{"mcpServer/elicitation/request", `{"action":"decline","content":null}`, true},
		{"mcpServer/elicitation/request", `{"action":"accept","content":{"name":"Ada"}}`, true},
		{"mcpServer/elicitation/request", `{"action":"accept"}`, false},
		{"item/tool/call", `{}`, false},
	}
	for _, test := range tests {
		if got := validInteractionResponse(test.method, json.RawMessage(test.response)); got != test.valid {
			t.Errorf("%s valid=%t, want %t", test.method, got, test.valid)
		}
	}
}

func TestPublicInteractionProjectsQuestionsAndRequestedPermissionNames(t *testing.T) {
	input := publicInteraction("session-1", NativeInteraction{ID: "input", Method: "item/tool/requestUserInput", Params: json.RawMessage(`{"questions":[{"id":"choice","header":"Choice","question":"Continue?","isSecret":true,"options":[{"label":"yes","description":"continue"}]}]}`)})
	if len(input.Questions) != 1 || input.Questions[0].ID != "choice" || !input.Questions[0].Sensitive || input.AllowedDecisions[0] != "answer" {
		t.Fatalf("input projection = %#v", input)
	}
	permissions := publicInteraction("session-1", NativeInteraction{ID: "permissions", Method: "item/permissions/requestApproval", Params: json.RawMessage(`{"permissions":{"fileSystem":{},"network":{}}}`)})
	if len(permissions.Permissions) != 2 || permissions.AllowedDecisions[0] != "deny" {
		t.Fatalf("permission projection = %#v", permissions)
	}
}

func TestCreatedControlStateRejectsMalformedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crew-codex.json")
	if err := os.WriteFile(path, []byte(`{"created":{"bad":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlState(path); err == nil {
		t.Fatal("malformed state was accepted")
	}
}

func TestRPCIDKeysKeepStringAndNumericSpacesSeparate(t *testing.T) {
	if rpcIDKey(json.RawMessage(`1`)) == rpcIDKey(json.RawMessage(`"1"`)) {
		t.Fatal("numeric and string RPC ids collided")
	}
}
