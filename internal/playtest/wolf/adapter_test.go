package wolf

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigApplicationDefaultsAndTrims(t *testing.T) {
	config := Config{
		SSHHost: "example", TargetPort: 48190, StreamHost: "stream",
		Moonlight: "moonlight", MoonlightConfig: "moonlight-config", State: "state",
	}
	if err := config.validate(); err != nil {
		t.Fatalf("validate default application: %v", err)
	}
	if config.Application != defaultApplication {
		t.Fatalf("default application = %q, want %q", config.Application, defaultApplication)
	}
	config.Application = "  Test ball  "
	if err := config.validate(); err != nil {
		t.Fatalf("validate configured application: %v", err)
	}
	if config.Application != "Test ball" {
		t.Fatalf("configured application = %q", config.Application)
	}
}

func TestDecodeRemoteResponse(t *testing.T) {
	result, err := decodeRemoteResponse("status", []byte(`{"ok":true,"result":{"lease":{"id":"lease-1"}}}`))
	if err != nil {
		t.Fatalf("decode success: %v", err)
	}
	lease, ok := result["lease"].(map[string]any)
	if !ok || lease["id"] != "lease-1" {
		t.Fatalf("unexpected decoded result: %#v", result)
	}
	if _, err := decodeRemoteResponse("input", []byte(`{"ok":false,"error":"Unknown or released lease"}`)); err == nil || !strings.Contains(err.Error(), "Unknown or released lease") {
		t.Fatalf("target failure was not preserved: %v", err)
	}
	if _, err := decodeRemoteResponse("status", []byte(`not-json`)); err == nil {
		t.Fatal("malformed target response was accepted")
	}
}

func TestInputCancellationUsesPreallocatedActionIDAndAwaitsTargetCleanup(t *testing.T) {
	directory := t.TempDir()
	inputStarted := make(chan map[string]any, 1)
	allowInputReturn := make(chan struct{})
	var mu sync.Mutex
	var cancelAction string
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: directory,
		remoteCall: func(_ context.Context, operation string, values map[string]any) (map[string]any, error) {
			switch operation {
			case "input":
				inputStarted <- values
				<-allowInputReturn
				return map[string]any{"event": "input", "cancelled": true}, nil
			case "cancel":
				mu.Lock()
				cancelAction, _ = values["action_id"].(string)
				mu.Unlock()
				close(allowInputReturn)
				return map[string]any{"cancel_requested": true}, nil
			default:
				return nil, errors.New("unexpected target operation")
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result map[string]any
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := adapter.Input(ctx, "lease-1", []map[string]any{{"kind": "wait", "ms": 10}})
		finished <- outcome{result: result, err: err}
	}()
	input := <-inputStarted
	actionID, _ := input["action_id"].(string)
	if actionID == "" {
		t.Fatal("input did not reserve an action ID")
	}
	cancel()
	select {
	case returned := <-finished:
		if !errors.Is(returned.err, context.Canceled) {
			t.Fatalf("input cancellation error = %v, want context.Canceled", returned.err)
		}
		if returned.result["cancelled"] != true {
			t.Fatalf("cancelled receipt was not returned: %#v", returned.result)
		}
	case <-time.After(time.Second):
		t.Fatal("input returned before target cleanup completed")
	}
	mu.Lock()
	defer mu.Unlock()
	if cancelAction != actionID {
		t.Fatalf("cancel action ID = %q, input action ID = %q", cancelAction, actionID)
	}
}

func TestInputCancellationReportsTargetReleaseFailure(t *testing.T) {
	inputStarted := make(chan struct{}, 1)
	allowInputReturn := make(chan struct{})
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: t.TempDir(),
		remoteCall: func(_ context.Context, operation string, _ map[string]any) (map[string]any, error) {
			switch operation {
			case "input":
				inputStarted <- struct{}{}
				<-allowInputReturn
				return map[string]any{"cancelled": true, "release_errors": []any{"key-up delivery timed out"}}, nil
			case "cancel":
				close(allowInputReturn)
				return map[string]any{"cancel_requested": true}, nil
			default:
				return nil, errors.New("unexpected target operation")
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		receipt map[string]any
		err     error
	}, 1)
	go func() {
		receipt, err := adapter.Input(ctx, "lease-1", []map[string]any{{"kind": "wait", "ms": 10}})
		result <- struct {
			receipt map[string]any
			err     error
		}{receipt, err}
	}()
	<-inputStarted
	cancel()
	returned := <-result
	if !errors.Is(returned.err, ErrCleanupUncertain) {
		t.Fatalf("cancel error = %v, want cleanup uncertainty", returned.err)
	}
	if errors.Is(returned.err, context.Canceled) {
		t.Fatalf("cleanup uncertainty must not look like clean cancellation: %v", returned.err)
	}
	if returned.receipt["release_errors"] == nil {
		t.Fatalf("cleanup receipt was not returned: %#v", returned.receipt)
	}
}

func TestInputCancellationPropagatesReceiptJournalFailure(t *testing.T) {
	inputStarted := make(chan struct{}, 1)
	allowInputReturn := make(chan struct{})
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: filepath.Join(t.TempDir(), "missing-artifact-directory"),
		remoteCall: func(_ context.Context, operation string, _ map[string]any) (map[string]any, error) {
			switch operation {
			case "input":
				inputStarted <- struct{}{}
				<-allowInputReturn
				return map[string]any{"cancelled": true}, nil
			case "cancel":
				close(allowInputReturn)
				return map[string]any{"cancel_requested": true}, nil
			default:
				return nil, errors.New("unexpected target operation")
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		receipt map[string]any
		err     error
	}, 1)
	go func() {
		receipt, err := adapter.Input(ctx, "lease-1", []map[string]any{{"kind": "wait", "ms": 10}})
		result <- struct {
			receipt map[string]any
			err     error
		}{receipt, err}
	}()
	<-inputStarted
	cancel()
	returned := <-result
	if !os.IsNotExist(returned.err) {
		t.Fatalf("cancel error = %v, want artifact journal failure", returned.err)
	}
	if returned.receipt["cancelled"] != true {
		t.Fatalf("receipt was lost with journal failure: %#v", returned.receipt)
	}
}

func TestRecoveredReleaseNeverClaimsLocalCaptureOwnership(t *testing.T) {
	var request map[string]any
	adapter := &Adapter{remoteCall: func(_ context.Context, operation string, values map[string]any) (map[string]any, error) {
		if operation != "release" {
			return nil, errors.New("unexpected target operation")
		}
		request = values
		return map[string]any{"released": true}, nil
	}}
	result, err := adapter.Release(context.Background(), "saved-lease")
	if err != nil {
		t.Fatalf("release saved lease: %v", err)
	}
	if request["lease_id"] != "saved-lease" {
		t.Fatalf("target release used %#v, want saved lease", request)
	}
	if result["local_capture"] != "not_owned_after_recovery" || result["local_capture_stopped"] != false {
		t.Fatalf("recovered release claimed local cleanup: %#v", result)
	}
}

func TestReleaseReportsEvidenceFailureWithoutMisstatingCleanup(t *testing.T) {
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: filepath.Join(t.TempDir(), "missing-artifact-directory"),
		remoteCall: func(_ context.Context, operation string, _ map[string]any) (map[string]any, error) {
			if operation != "release" {
				return nil, errors.New("unexpected target operation")
			}
			return map[string]any{"released": true}, nil
		},
	}
	result, err := adapter.Release(context.Background(), "lease-1")
	if err == nil {
		t.Fatal("release receipt journal failure was hidden")
	}
	if result["released"] != true || result["local_capture_stopped"] != true {
		t.Fatalf("cleanup success was lost: %#v", result)
	}
	evidenceErr, _ := result["evidence_error"].(string)
	if !strings.Contains(evidenceErr, "persist local release receipt") {
		t.Fatalf("receipt did not distinguish evidence failure: %#v", result)
	}
}

func TestLaunchFirefoxUsesTargetOwnedOperation(t *testing.T) {
	var operation string
	var request map[string]any
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: t.TempDir(),
		remoteCall: func(_ context.Context, op string, values map[string]any) (map[string]any, error) {
			operation, request = op, values
			return map[string]any{"lobby_id": "firefox-lobby"}, nil
		},
	}
	result, err := adapter.LaunchFirefox(context.Background(), "lease-1", "http://game.test:37300/")
	if err != nil {
		t.Fatalf("launch Firefox: %v", err)
	}
	if operation != "launch" || request["lease_id"] != "lease-1" || request["url"] != "http://game.test:37300/" {
		t.Fatalf("target request = %q %#v", operation, request)
	}
	if result["lobby_id"] != "firefox-lobby" {
		t.Fatalf("launch result = %#v", result)
	}
}

func TestStopManagedProcessStopsItsProcessGroup(t *testing.T) {
	command := exec.Command("sh", "-c", "sleep 30 & wait")
	configureProcess(command)
	if err := command.Start(); err != nil {
		t.Fatalf("start process group: %v", err)
	}
	started := time.Now()
	stopManagedProcess(newManagedProcess(command, nil, nil))
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("process group cleanup took %s", elapsed)
	}
}

func TestCloseIsSafeWhenCalledConcurrently(t *testing.T) {
	adapter, err := New(Config{
		SSHHost: "example", TargetPort: 48190, StreamHost: "stream",
		Moonlight: "moonlight", MoonlightConfig: t.TempDir(), State: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			errors <- adapter.Close()
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("close adapter: %v", err)
		}
	}
}
