package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type notifyingWriter struct {
	strings.Builder
	wrote chan struct{}
	once  sync.Once
}

func (w *notifyingWriter) Write(value []byte) (int, error) {
	n, err := w.Builder.Write(value)
	if n > 0 {
		w.once.Do(func() { close(w.wrote) })
	}
	return n, err
}

func TestResultRoutesCommandAndReturnsServiceResult(t *testing.T) {
	var got Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"session_id":"s-1"}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Result(context.Background(), Request{Op: "start", Game: "space"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Op != "start" || got.Game != "space" {
		t.Fatalf("request = %+v", got)
	}
	if string(result) != `{"session_id":"s-1"}` {
		t.Fatalf("result = %s", result)
	}
}

func TestResultReportsServiceFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unknown session"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Result(context.Background(), Request{Op: "observe", SessionID: "missing"})
	var commandError *CommandError
	if !errors.As(err, &commandError) || commandError.Message != "unknown session" {
		t.Fatalf("error = %v", err)
	}
}

func TestServeMCPReturnsObserveImage(t *testing.T) {
	path := t.TempDir() + "/frame.png"
	// A one-pixel PNG is enough to verify MCP base64 image projection.
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLqGQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, png, 0600); err != nil {
		t.Fatal(err)
	}
	observed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Op != "observe" || request.SessionID != "s-1" {
			t.Fatalf("request = %+v", request)
		}
		close(observed)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"path":` + quote(path) + `,"at":"now"}}`))
	}))
	defer server.Close()
	service, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	defer writer.Close()
	output := notifyingWriter{wrote: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ServeMCP(context.Background(), input, &output, service) }()
	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"observe","arguments":{"session_id":"s-1"}}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("observe did not reach service")
	}
	select {
	case <-output.wrote:
	case <-time.After(time.Second):
		t.Fatal("observe response was not written")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("responses = %q", output.String())
	}
	var response struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"content"`
		} `json:"result"`
	}
	for _, line := range lines {
		var envelope struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.ID == 2 {
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(response.Result.Content) != 2 || response.Result.Content[1].Type != "image" {
		t.Fatalf("content = %+v", response.Result.Content)
	}
	if response.Result.Content[1].Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatal("MCP image data did not preserve observed file")
	}
}

func TestServeMCPDoesNotSerializeStatusBehindStart(t *testing.T) {
	startEntered := make(chan struct{})
	statusCalled := make(chan struct{})
	releaseStart := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		switch request.Op {
		case "start":
			close(startEntered)
			<-releaseStart
		case "status":
			close(statusCalled)
		default:
			t.Fatalf("command op = %q", request.Op)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	service, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	defer writer.Close()
	var output strings.Builder
	done := make(chan error, 1)
	go func() { done <- ServeMCP(context.Background(), input, &output, service) }()
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start","arguments":{"game":"space"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","arguments":{}}}`,
	} {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("start did not reach service")
	}
	select {
	case <-statusCalled:
	case <-time.After(time.Second):
		t.Fatal("status waited for start to complete")
	}
	close(releaseStart)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP server did not finish")
	}
}

func TestServeMCPCancelledNotificationCancelsActiveInput(t *testing.T) {
	inputStarted := make(chan struct{})
	inputCancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Op != "input" {
			t.Fatalf("command op = %q, want input", request.Op)
		}
		close(inputStarted)
		<-r.Context().Done()
		close(inputCancelled)
	}))
	defer server.Close()
	service, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	var output strings.Builder
	done := make(chan error, 1)
	go func() { done <- ServeMCP(context.Background(), reader, &output, service) }()
	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":"input-1","method":"tools/call","params":{"name":"input","arguments":{"session_id":"s-1","steps":[]}}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inputStarted:
	case <-time.After(time.Second):
		t.Fatal("input did not reach service")
	}
	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"input-1"}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inputCancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation notification did not cancel input request context")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP server did not finish after input cancellation")
	}
}

func TestToolResultReturnsLatestCheckpointPNG(t *testing.T) {
	path := t.TempDir() + "/checkpoint.png"
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLqGQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, png, 0600); err != nil {
		t.Fatal(err)
	}
	result := toolResult("script", json.RawMessage(`{"checkpoint":{"data":{"path":`+quote(path)+`}}}`))
	content := result["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	image, ok := content[1].(map[string]string)
	if !ok || image["type"] != "image" || image["mimeType"] != "image/png" {
		t.Fatalf("checkpoint image = %#v", content[1])
	}
	withoutPath := toolResult("script", json.RawMessage(`{"checkpoint":{"data":{"label":"saved"}}}`))
	if got := len(withoutPath["content"].([]any)); got != 1 {
		t.Fatalf("arbitrary checkpoint data added %d content blocks", got)
	}
}

func TestNewRejectsNonLoopbackURL(t *testing.T) {
	if _, err := New("https://playtest.example", nil); err == nil {
		t.Fatal("New accepted non-loopback HTTPS URL")
	}
}

func TestNewUsesLaunchCompatibleDefaultTimeout(t *testing.T) {
	client, err := New(DefaultURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.http.Timeout != 120*time.Second {
		t.Fatalf("timeout = %s, want 120s", client.http.Timeout)
	}
}

func TestNormalizeStepsAcceptsArrayOrStepsObject(t *testing.T) {
	for _, input := range []string{
		`[{"kind":"key","key":"SPACE"}]`,
		`{"steps":[{"kind":"key","key":"SPACE"}]}`,
	} {
		steps, err := NormalizeSteps(json.RawMessage(input))
		if err != nil {
			t.Fatalf("NormalizeSteps(%s): %v", input, err)
		}
		if !strings.HasPrefix(string(steps), "[") {
			t.Fatalf("steps = %s", steps)
		}
	}
	if _, err := NormalizeSteps(json.RawMessage(`{"steps":{}}`)); err == nil {
		t.Fatal("NormalizeSteps accepted a non-array steps member")
	}
}

func TestMCPInteractionDefaultsOptionsAndPreservesSuppliedOptions(t *testing.T) {
	defaultCommand, err := mcpCommand("interaction", map[string]json.RawMessage{"session_id": json.RawMessage(`"s-1"`)})
	if err != nil {
		t.Fatal(err)
	}
	if defaultCommand.Op != "interaction" || defaultCommand.SessionID != "s-1" || string(defaultCommand.Data) != "{}" {
		t.Fatalf("default command = %+v", defaultCommand)
	}
	options := json.RawMessage(`{"mode":"cursor","x":0.25,"y":0.75,"aspect":1.777}`)
	command, err := mcpCommand("interaction", map[string]json.RawMessage{"session_id": json.RawMessage(`"s-1"`), "data": options})
	if err != nil {
		t.Fatal(err)
	}
	if string(command.Data) != string(options) {
		t.Fatalf("options = %s, want %s", command.Data, options)
	}
}

func TestMCPToolSchemasHaveObjectRoots(t *testing.T) {
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%s input schema = %#v, want an object root", name, schema)
		}
	}
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
