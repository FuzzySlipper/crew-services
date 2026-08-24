package codexadapter

import (
	"context"
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
