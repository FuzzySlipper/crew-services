package reviewdsh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"crew-services/internal/review"
)

func TestRuntimeAcquireRunRelease(t *testing.T) {
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, got)
		w.Header().Set("Content-Type", "application/json")
		switch got.Action {
		case "acquire":
			_, _ = w.Write([]byte(`{"worker_id":"opaque-worker"}`))
		case "run":
			_, _ = w.Write([]byte(`{"completion":{"verdict":"looks_good","notes":"clean"}}`))
		case "release":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, `{"error":"unexpected action"}`, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	runtime := newRuntime(t, server.URL, 1)
	worker, err := runtime.Acquire(context.Background(), review.TaskKey{ProjectID: "dsh-crew", TaskID: 7609}, "ignored", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	if err := runtime.Run(context.Background(), worker, "review this", func(got review.Completion) error {
		calls++
		if got.Verdict != "looks_good" {
			t.Fatalf("completion = %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("completion callbacks = %d, want one", calls)
	}
	if err := runtime.Release(context.Background(), worker); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Release(context.Background(), worker); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Action != "acquire" || requests[0].Workspace != "/repo" || requests[0].OperationID == "" {
		t.Fatalf("acquire request = %+v", requests[0])
	}
	if requests[1].Action != "run" || requests[1].WorkerID != "opaque-worker" || requests[1].RunID == "" || requests[1].Prompt != "review this" {
		t.Fatalf("run request = %+v", requests[1])
	}
	if requests[2].Action != "release" || requests[2].WorkerID != "opaque-worker" {
		t.Fatalf("release request = %+v", requests[2])
	}
}

func TestRuntimeEnforcesLocalCapacity(t *testing.T) {
	var acquires atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Action == "acquire" {
			acquires.Add(1)
			_, _ = w.Write([]byte(`{"worker_id":"opaque"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	runtime := newRuntime(t, server.URL, 1)
	if _, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo"); !errors.Is(err, review.ErrCapacity) {
		t.Fatalf("second acquire error = %v, want ErrCapacity", err)
	}
	if got := acquires.Load(); got != 1 {
		t.Fatalf("remote acquires = %d, want one", got)
	}
}

func TestRuntimeRejectsBusyRunAndRelease(t *testing.T) {
	started := make(chan struct{})
	releaseRun := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		_ = json.NewDecoder(r.Body).Decode(&got)
		switch got.Action {
		case "acquire":
			_, _ = w.Write([]byte(`{"worker_id":"opaque"}`))
		case "run":
			close(started)
			<-releaseRun
			_, _ = w.Write([]byte(`{"completion":{"verdict":"looks_good"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	runtime := newRuntime(t, server.URL, 1)
	worker, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runtime.Run(context.Background(), worker, "first", func(review.Completion) error { return nil })
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first run did not reach DSH")
	}
	if err := runtime.Run(context.Background(), worker, "second", func(review.Completion) error { return nil }); err == nil {
		t.Fatal("second run succeeded while worker was busy")
	}
	if err := runtime.Release(context.Background(), worker); err == nil {
		t.Fatal("release succeeded while worker was busy")
	}
	close(releaseRun)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCancellationDoesNotDeliverOrRetry(t *testing.T) {
	started := make(chan struct{})
	var runs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		_ = json.NewDecoder(r.Body).Decode(&got)
		switch got.Action {
		case "acquire":
			_, _ = w.Write([]byte(`{"worker_id":"opaque"}`))
		case "run":
			runs.Add(1)
			close(started)
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	runtime := newRuntime(t, server.URL, 1)
	worker, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var callbacks atomic.Int32
	go func() {
		done <- runtime.Run(ctx, worker, "review", func(review.Completion) error { callbacks.Add(1); return nil })
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("run did not reach DSH")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancellation", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("run requests = %d, want one", got)
	}
	if got := callbacks.Load(); got != 0 {
		t.Fatalf("callbacks = %d, want none", got)
	}
}

func TestRuntimeRejectsMalformedOrInvalidCompletion(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":       `{`,
		"missing":         `{}`,
		"invalid verdict": `{"completion":{"verdict":"maybe"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got request
				_ = json.NewDecoder(r.Body).Decode(&got)
				if got.Action == "acquire" {
					_, _ = w.Write([]byte(`{"worker_id":"opaque"}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			runtime := newRuntime(t, server.URL, 1)
			worker, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo")
			if err != nil {
				t.Fatal(err)
			}
			var callbacks int
			if err := runtime.Run(context.Background(), worker, "review", func(review.Completion) error { callbacks++; return nil }); err == nil {
				t.Fatal("run succeeded")
			}
			if callbacks != 0 {
				t.Fatalf("callbacks = %d, want none", callbacks)
			}
		})
	}
}

func TestNewRequiresUsableConfiguration(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty URL":        {Capacity: 1},
		"invalid URL":      {URL: "://bad", Capacity: 1},
		"invalid capacity": {URL: "http://127.0.0.1:3080/runtime", Capacity: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("New succeeded")
			}
		})
	}
}

func TestRuntimeClassifiesBackendLossForDurableRetry(t *testing.T) {
	for name, handler := range map[string]http.Handler{
		"missing plugin": http.NotFoundHandler(),
		"stale worker": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"reviewer worker is no longer active"}`))
		}),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			runtime := newRuntime(t, server.URL, 1)
			_, err := runtime.Acquire(context.Background(), review.TaskKey{}, "", "/repo")
			if !errors.Is(err, review.ErrRuntimeUnavailable) {
				t.Fatalf("Acquire error = %v, want ErrRuntimeUnavailable", err)
			}
		})
	}
}

func newRuntime(t *testing.T, endpoint string, capacity int) *Runtime {
	t.Helper()
	runtime, err := New(Config{URL: endpoint, Capacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
