package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"
)

func TestStdioAppServerCloseStopsChildPromptly(t *testing.T) {
	// The shell replies to initialize and then remains attached to stdin. This
	// proves Close unblocks the child-exit path rather than waiting for timeout.
	client, err := StartStdioAppServer("sh", []string{"-c", "while IFS= read -r line; do printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\\n'; done"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("close took %s", elapsed)
	}
}

func TestStdioAppServerChildExitFailsRequest(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell unavailable")
	}
	client, err := StartStdioAppServer("sh", []string{"-c", "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err == nil {
		t.Fatal("initialize succeeded after child exit")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListThreads(ctx); err == nil {
		t.Fatal("request succeeded after close")
	}
}

func TestStdioAppServerFailCancelsDynamicToolContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &StdioAppServer{toolContext: ctx, toolCancel: cancel, pending: map[string]chan rpcResponse{}, interactions: map[string]pendingInteraction{}, done: make(chan struct{}), handshakeDone: make(chan struct{})}
	client.fail(context.Canceled)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("dynamic tool context was not cancelled")
	}
}

func TestStdioAppServerBlockingDynamicToolDoesNotBlockReadLoop(t *testing.T) {
	toolContext, toolCancel := context.WithCancel(context.Background())
	client := &StdioAppServer{
		stdin:         &testWriteCloser{write: func(p []byte) (int, error) { return len(p), nil }},
		pending:       map[string]chan rpcResponse{},
		interactions:  map[string]pendingInteraction{},
		done:          make(chan struct{}),
		handshakeDone: make(chan struct{}),
		toolContext:   toolContext,
		toolCancel:    toolCancel,
	}

	toolStarted := make(chan struct{})
	toolCanceled := make(chan struct{})
	client.SetDynamicToolHandler(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(toolStarted)
		<-ctx.Done()
		close(toolCanceled)
		return nil, ctx.Err()
	})

	response := make(chan rpcResponse, 1)
	client.mu.Lock()
	client.pending["n:42"] = response
	client.mu.Unlock()

	frameReturned := make(chan struct{})
	go func() {
		client.handleFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"item/tool/call","params":{"callId":"call-1"}}`))
		close(frameReturned)
	}()
	select {
	case <-toolStarted:
	case <-time.After(time.Second):
		t.Fatal("dynamic tool handler did not start")
	}
	select {
	case <-frameReturned:
	case <-time.After(time.Second):
		t.Fatal("read loop remained blocked by dynamic tool handler")
	}

	client.handleFrame([]byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`))
	select {
	case result, ok := <-response:
		if !ok || string(result.result) != `{"ok":true}` {
			t.Fatalf("unrelated response=%+v open=%t", result, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated response was not processed while tool handler blocked")
	}

	client.mu.Lock()
	client.interactions["interaction-1"] = pendingInteraction{id: json.RawMessage(`7`), method: "item/approval"}
	client.mu.Unlock()
	client.handleFrame([]byte(`{"jsonrpc":"2.0","method":"serverRequest/resolved","params":{"requestId":7}}`))
	if interactions := client.Interactions(); len(interactions) != 0 {
		t.Fatalf("unrelated notification was not processed while tool handler blocked: %+v", interactions)
	}

	client.fail(errors.New("child closed"))
	select {
	case <-toolCanceled:
	case <-time.After(time.Second):
		t.Fatal("child failure did not cancel dynamic tool handler context")
	}
}

func TestStdioAppServerDynamicToolWriteFailureClosesPendingState(t *testing.T) {
	tests := []struct {
		name  string
		write func([]byte) (int, error)
	}{
		{name: "write error", write: func(p []byte) (int, error) { return 0, errors.New("stdin closed") }},
		{name: "short write", write: func(p []byte) (int, error) { return len(p) - 1, nil }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			toolContext, toolCancel := context.WithCancel(context.Background())
			stdin := &testWriteCloser{write: test.write}
			client := &StdioAppServer{
				stdin:         stdin,
				pending:       map[string]chan rpcResponse{},
				interactions:  map[string]pendingInteraction{},
				done:          make(chan struct{}),
				handshakeDone: make(chan struct{}),
				toolContext:   toolContext,
				toolCancel:    toolCancel,
			}
			pending := make(chan rpcResponse)
			client.pending["n:42"] = pending
			client.SetDynamicToolHandler(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"success":true}`), nil
			})

			client.handleFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"item/tool/call","params":{"callId":"call-1"}}`))
			select {
			case <-client.done:
			case <-time.After(time.Second):
				t.Fatal("dynamic tool write failure did not fail the App Server")
			}
			select {
			case _, ok := <-pending:
				if ok {
					t.Fatal("pending response was delivered instead of closed")
				}
			case <-time.After(time.Second):
				t.Fatal("pending response remained open after dynamic tool write failure")
			}
			client.mu.Lock()
			terminalErr := client.err
			client.mu.Unlock()
			if terminalErr == nil {
				t.Fatal("dynamic tool write failure did not record terminal error")
			}
		})
	}
}

func TestStdioAppServerSendRequestRejectsFailedAndShortWrites(t *testing.T) {
	tests := []struct {
		name  string
		write func([]byte) (int, error)
	}{
		{name: "write error", write: func([]byte) (int, error) { return 0, errors.New("stdin closed") }},
		{name: "short write", write: func(p []byte) (int, error) { return len(p) - 1, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &StdioAppServer{
				stdin:         &testWriteCloser{write: test.write},
				pending:       map[string]chan rpcResponse{},
				interactions:  map[string]pendingInteraction{},
				done:          make(chan struct{}),
				handshakeDone: make(chan struct{}),
			}
			var target map[string]any
			if err := client.sendRequest(context.Background(), "test/request", map[string]any{}, &target); err == nil {
				t.Fatal("sendRequest accepted incomplete write")
			}
			client.mu.Lock()
			pending := len(client.pending)
			client.mu.Unlock()
			if pending != 0 {
				t.Fatalf("failed sendRequest left %d pending requests", pending)
			}
		})
	}
}

func TestStdioAppServerInitializedNotificationRejectsFailedAndShortWrites(t *testing.T) {
	tests := []struct {
		name  string
		write func([]byte) (int, error)
	}{
		{name: "write error", write: func([]byte) (int, error) { return 0, errors.New("stdin closed") }},
		{name: "short write", write: func(p []byte) (int, error) { return len(p) - 1, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &StdioAppServer{
				stdin:         &testWriteCloser{write: test.write},
				pending:       map[string]chan rpcResponse{},
				interactions:  map[string]pendingInteraction{},
				done:          make(chan struct{}),
				handshakeDone: make(chan struct{}),
			}
			if err := client.notifyInitialized(); err == nil {
				t.Fatal("initialized notification accepted incomplete write")
			}
		})
	}
}

func TestStdioAppServerInteractionResponseRejectsFailedAndShortWrites(t *testing.T) {
	tests := []struct {
		name  string
		write func([]byte) (int, error)
	}{
		{name: "write error", write: func([]byte) (int, error) { return 0, errors.New("stdin closed") }},
		{name: "short write", write: func(p []byte) (int, error) { return len(p) - 1, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &StdioAppServer{
				stdin:         &testWriteCloser{write: test.write},
				pending:       map[string]chan rpcResponse{},
				interactions:  map[string]pendingInteraction{},
				done:          make(chan struct{}),
				handshakeDone: make(chan struct{}),
			}
			client.interactions["interaction-1"] = pendingInteraction{id: json.RawMessage(`7`), method: "item/approval"}
			if err := client.RespondInteraction(context.Background(), "interaction-1", "item/approval", json.RawMessage(`{"decision":"accept"}`)); err == nil {
				t.Fatal("interaction response accepted incomplete write")
			}
			if len(client.Interactions()) != 1 {
				t.Fatal("failed interaction response removed the pending interaction")
			}
		})
	}
}

func TestStdioAppServerStartTurnUsesClientMessageIdentity(t *testing.T) {
	writes := make(chan []byte, 1)
	client := &StdioAppServer{
		stdin: &testWriteCloser{write: func(value []byte) (int, error) {
			writes <- append([]byte(nil), value...)
			return len(value), nil
		}},
		pending:           map[string]chan rpcResponse{},
		interactions:      map[string]pendingInteraction{},
		done:              make(chan struct{}),
		handshakeDone:     make(chan struct{}),
		handshakeComplete: true,
	}
	result := make(chan struct {
		turn StartedTurn
		err  error
	}, 1)
	go func() {
		turn, err := client.StartTurn(context.Background(), "thread-1", "first prompt", "crew-delivery:delivery-1")
		result <- struct {
			turn StartedTurn
			err  error
		}{turn, err}
	}()

	var request struct {
		Method string `json:"method"`
		Params struct {
			ThreadID            string `json:"threadId"`
			ClientUserMessageID string `json:"clientUserMessageId"`
			Input               []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	select {
	case wire := <-writes:
		if err := json.Unmarshal(wire, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("turn/start was not written")
	}
	if request.Method != "turn/start" || request.Params.ThreadID != "thread-1" || request.Params.ClientUserMessageID != "crew-delivery:delivery-1" || len(request.Params.Input) != 1 || request.Params.Input[0].Type != "text" || request.Params.Input[0].Text != "first prompt" {
		t.Fatalf("turn/start request = %+v", request)
	}
	client.handleFrame([]byte(`{"jsonrpc":"2.0","id":1,"result":{"turn":{"id":"turn-1"}}}`))
	select {
	case value := <-result:
		if value.err != nil || value.turn.ID != "turn-1" {
			t.Fatalf("turn/start result = %+v, %v", value.turn, value.err)
		}
	case <-time.After(time.Second):
		t.Fatal("turn/start did not receive its response")
	}
}

type testWriteCloser struct {
	write func([]byte) (int, error)
}

func (w *testWriteCloser) Write(p []byte) (int, error) { return w.write(p) }

func (w *testWriteCloser) Close() error { return nil }

var _ io.WriteCloser = (*testWriteCloser)(nil)

func TestStdioAppServerWaitsForInitializedBeforeConcurrentRequest(t *testing.T) {
	client, err := StartStdioAppServer("sh", []string{"-c", `ready=0
while IFS= read -r line; do
  case "$line" in
    *'"id":1'*) sleep 0.05; printf '{"jsonrpc":"2.0","id":1,"result":{}}\n' ;;
    *'"method":"initialized"'*) ready=1 ;;
    *'"id":2'*) if [ "$ready" = 1 ]; then printf '{"jsonrpc":"2.0","id":2,"result":{"data":[]}}\n'; fi ;;
  esac
done`})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialized := make(chan error, 1)
	go func() { initialized <- client.Initialize(ctx) }()
	waitForHandshakeStart(t, client)
	listed := make(chan error, 1)
	go func() { _, err := client.ListThreads(ctx); listed <- err }()
	select {
	case err := <-listed:
		t.Fatalf("thread/list completed before initialized notification: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := <-initialized; err != nil {
		t.Fatal(err)
	}
	if err := <-listed; err != nil {
		t.Fatal(err)
	}
}

func TestStdioAppServerCloseDuringDelayedInitializeIsPrompt(t *testing.T) {
	client, err := StartStdioAppServer("sh", []string{"-c", "IFS= read -r line; sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialized := make(chan error, 1)
	go func() { initialized <- client.Initialize(ctx) }()
	waitForHandshakeStart(t, client)
	started := time.Now()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("close took %s", elapsed)
	}
	select {
	case err := <-initialized:
		if err == nil {
			t.Fatal("initialize succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("initialize did not unblock after close")
	}
}

func waitForHandshakeStart(t *testing.T, client *StdioAppServer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		started := client.handshakeStarted
		client.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("initialize did not start")
}

func TestTurnCompletionWaitsForExactTurnAndExitFailsWaiter(t *testing.T) {
	client := &StdioAppServer{turnWaiters: map[string]chan TurnCompletion{}, completedTurns: map[string]TurnCompletion{}, ephemeralThreads: map[string]struct{}{"thread": {}}, done: make(chan struct{}), interactions: map[string]pendingInteraction{}, pending: map[string]chan rpcResponse{}, handshakeDone: make(chan struct{})}
	first := make(chan error, 1)
	go func() { _, err := client.WaitTurn(context.Background(), "thread", "one"); first <- err }()
	time.Sleep(time.Millisecond)
	client.handleFrame([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"two","status":"completed"}}}`))
	select {
	case err := <-first:
		t.Fatalf("wrong turn settled waiter: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	client.handleFrame([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"one","status":"completed"}}}`))
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { _, err := client.WaitTurn(context.Background(), "thread", "three"); wait <- err }()
	time.Sleep(time.Millisecond)
	client.fail(errors.New("child exited"))
	if err := <-wait; err == nil {
		t.Fatal("exit did not fail exact waiter")
	}
}

func TestUntrackedCompletionIsIgnoredButTrackedPreWaitIsCached(t *testing.T) {
	client := &StdioAppServer{turnWaiters: map[string]chan TurnCompletion{}, completedTurns: map[string]TurnCompletion{}, ephemeralThreads: map[string]struct{}{"ephemeral": {}}, done: make(chan struct{}), interactions: map[string]pendingInteraction{}, pending: map[string]chan rpcResponse{}, handshakeDone: make(chan struct{})}
	client.handleFrame([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"ordinary","turn":{"id":"one","status":"completed"}}}`))
	if len(client.completedTurns) != 0 {
		t.Fatalf("ordinary completion cached: %#v", client.completedTurns)
	}
	client.handleFrame([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"ephemeral","turn":{"id":"two","status":"completed"}}}`))
	done, err := client.WaitTurn(context.Background(), "ephemeral", "two")
	if err != nil || done.TurnID != "two" {
		t.Fatalf("cached ephemeral completion=%+v err=%v", done, err)
	}
}
