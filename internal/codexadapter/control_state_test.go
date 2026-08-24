package codexadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestCrewDynamicToolsResolveCurrentCallerBindingAndUseStableCallOperation(t *testing.T) {
	fabric := newFakeFabric()
	fabric.bindings["crew/caller"] = Binding{Address: "crew/caller", Bound: true, AdapterID: "crew-codex", TargetRef: "session-1", Generation: 3}
	fabric.bindings["crew/recipient"] = Binding{Address: "crew/recipient", Bound: true, AdapterID: "other", TargetRef: "session-2", Generation: 1, Capabilities: []string{"deliver_when_idle"}}
	controls := NewControls(fabric, Config{AdapterID: "crew-codex"})
	controls.lease = Lease{LeaseToken: "lease"}
	controls.mappings["session-1"] = Mapping{Address: "crew/caller", ThreadID: "native-1"}
	controls.persisted["create"] = persistedControlSession{Session: ControlSession{SessionID: "session-1"}, Mapping: Mapping{Address: "crew/caller", ThreadID: "native-1"}, ToolEnabled: true}
	result, err := controls.dynamicTool(context.Background(), json.RawMessage(`{"threadId":"native-1","turnId":"turn-1","callId":"call-1","tool":"crew_message","arguments":{"recipient":"crew/recipient","text":"hello"}}`))
	if err != nil || !contains(string(result), "message-"+toolOperation("native-1", "call-1")) {
		t.Fatalf("dynamic tool result=%s err=%v", result, err)
	}
	fabric.bindings["crew/caller"] = Binding{Address: "crew/caller", Bound: true, AdapterID: "other", TargetRef: "session-1"}
	if _, err := controls.dynamicTool(context.Background(), json.RawMessage(`{"threadId":"native-1","turnId":"turn-1","callId":"call-2","tool":"crew_directory","arguments":{}}`)); err == nil {
		t.Fatal("stale caller binding was accepted")
	}
}

func TestCrewDynamicToolRejectsMalformedAndUnroutableCalls(t *testing.T) {
	fabric := newFakeFabric()
	fabric.bindings["crew/caller"] = Binding{Address: "crew/caller", Bound: true, AdapterID: "crew-codex", TargetRef: "session-1", Generation: 3}
	fabric.bindings["crew/recipient"] = Binding{Address: "crew/recipient", Bound: true, AdapterID: "other", TargetRef: "session-2", Generation: 1}
	controls := NewControls(fabric, Config{AdapterID: "crew-codex"})
	controls.lease = Lease{LeaseToken: "lease"}
	controls.mappings["session-1"] = Mapping{Address: "crew/caller", ThreadID: "native-1"}
	controls.persisted["create"] = persistedControlSession{Session: ControlSession{SessionID: "session-1"}, Mapping: Mapping{Address: "crew/caller", ThreadID: "native-1"}, ToolEnabled: true}

	valid := func(tool string, arguments any) json.RawMessage {
		t.Helper()
		value := map[string]any{"threadId": "native-1", "turnId": "turn-1", "callId": "call-1", "tool": tool, "arguments": arguments}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	unknownTopLevel := func() json.RawMessage {
		value := map[string]any{"threadId": "native-1", "turnId": "turn-1", "callId": "call-1", "tool": "crew_directory", "arguments": map[string]any{}, "extra": true}
		encoded, _ := json.Marshal(value)
		return encoded
	}
	for _, test := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "null", raw: json.RawMessage(`null`)},
		{name: "array", raw: json.RawMessage(`[]`)},
		{name: "unknown top level", raw: unknownTopLevel()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := controls.dynamicTool(context.Background(), test.raw); err == nil {
				t.Fatalf("accepted malformed call %s", test.name)
			}
		})
	}

	for _, test := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "missing turn", raw: func() json.RawMessage {
			value := map[string]any{"threadId": "native-1", "callId": "call-1", "tool": "crew_directory", "arguments": map[string]any{}}
			encoded, _ := json.Marshal(value)
			return encoded
		}()},
		{name: "non-null namespace", raw: func() json.RawMessage {
			value := map[string]any{"threadId": "native-1", "turnId": "turn-1", "callId": "call-1", "namespace": "crew", "tool": "crew_directory", "arguments": map[string]any{}}
			encoded, _ := json.Marshal(value)
			return encoded
		}()},
		{name: "directory null arguments", raw: valid("crew_directory", nil)},
		{name: "directory unknown arguments", raw: valid("crew_directory", map[string]any{"unknown": true})},
		{name: "message unknown arguments", raw: valid("crew_message", map[string]any{"recipient": "crew/recipient", "text": "hello", "unknown": true})},
		{name: "message oversized", raw: valid("crew_message", map[string]any{"recipient": "crew/recipient", "text": strings.Repeat("x", 12_001)})},
		{name: "recipient without inbound capability", raw: valid("crew_message", map[string]any{"recipient": "crew/recipient", "text": "hello"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := controls.dynamicTool(context.Background(), test.raw); err == nil {
				t.Fatalf("accepted malformed call %s", test.name)
			}
		})
	}
}

func TestToolOperationSeparatesNulAndDelimiterTuples(t *testing.T) {
	tuples := [][2]string{
		{"a\x00b", "c"},
		{"a", "b\x00c"},
		{"a:b", "c"},
		{"a", "b:c"},
		{"", "a:b"},
		{"a:b", ""},
	}
	seen := make(map[string][2]string, len(tuples))
	for _, tuple := range tuples {
		operation := toolOperation(tuple[0], tuple[1])
		if previous, found := seen[operation]; found {
			t.Fatalf("tool operation collision: %q and %q both map to %q", previous, tuple, operation)
		}
		seen[operation] = tuple
		if operation != toolOperation(tuple[0], tuple[1]) {
			t.Fatalf("tool operation is not stable for %q", tuple)
		}
	}
}
