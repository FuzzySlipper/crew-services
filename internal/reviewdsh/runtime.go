// Package reviewdsh is the DSH-plugin backed reviewer runtime for crew-review.
package reviewdsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"crew-services/internal/review"
)

const DefaultURL = "http://127.0.0.1:3080/plugins/dsh-crew-messaging/reviewer-runtime"

// Config describes the loopback control route contributed by the DSH plugin.
// Model, provider, preset, and reviewer instructions are deliberately DSH-side
// configuration; this adapter only requests workers and submits review turns.
type Config struct {
	URL      string
	Capacity int
	Client   *http.Client
}

type Runtime struct {
	mu       sync.Mutex
	endpoint string
	client   *http.Client
	capacity int
	workers  map[*worker]struct{}
	reserved int
	next     uint64
	closed   bool
}

type worker struct {
	id         string
	busy       bool
	released   bool
	releasing  bool
	release    chan struct{}
	releaseErr error
}

type request struct {
	Action      string `json:"action"`
	OperationID string `json:"operation_id,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	WorkerID    string `json:"worker_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
}

type response struct {
	WorkerID   string             `json:"worker_id"`
	Completion *review.Completion `json:"completion"`
	Replayed   bool               `json:"replayed,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// New validates local configuration without contacting DSH. This keeps service
// startup deterministic while the plugin reports its own availability per job.
func New(cfg Config) (*Runtime, error) {
	endpoint := strings.TrimSpace(cfg.URL)
	if endpoint == "" {
		return nil, errors.New("DSH reviewer URL is required")
	}
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid DSH reviewer URL %q", endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("DSH reviewer URL must use http or https")
	}
	if cfg.Capacity < 1 {
		return nil, errors.New("DSH reviewer capacity must be positive")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Runtime{endpoint: endpoint, client: client, capacity: cfg.Capacity, workers: map[*worker]struct{}{}}, nil
}

func (r *Runtime) Acquire(ctx context.Context, _ review.TaskKey, _ string, workspace string) (review.Worker, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("DSH reviewer runtime is closed")
	}
	if len(r.workers)+r.reserved >= r.capacity {
		r.mu.Unlock()
		return nil, review.ErrCapacity
	}
	op := r.operationIDLocked("acquire")
	r.reserved++
	r.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			r.mu.Lock()
			r.reserved--
			r.mu.Unlock()
		}
	}()

	var reply response
	if err := r.call(ctx, request{Action: "acquire", OperationID: op, Workspace: workspace}, &reply); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reply.WorkerID) == "" {
		return nil, errors.New("DSH reviewer acquire response omitted worker_id")
	}
	w := &worker{id: reply.WorkerID}
	r.mu.Lock()
	r.reserved--
	reserved = false
	closed := r.closed
	if !closed {
		r.workers[w] = struct{}{}
	}
	r.mu.Unlock()
	if closed {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.call(cleanupCtx, request{Action: "release", WorkerID: w.id}, &response{})
		return nil, errors.New("DSH reviewer runtime closed while acquiring worker")
	}
	return w, nil
}

func (r *Runtime) Run(ctx context.Context, raw review.Worker, prompt string, complete func(review.Completion) error) error {
	if complete == nil {
		return errors.New("DSH reviewer completion callback is required")
	}
	w, err := r.beginRun(raw)
	if err != nil {
		return err
	}
	defer r.finishRun(w)

	r.mu.Lock()
	runID := r.operationIDLocked("run")
	r.mu.Unlock()
	var reply response
	if err := r.call(ctx, request{Action: "run", WorkerID: w.id, RunID: runID, Prompt: prompt}, &reply); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if reply.Completion == nil {
		return errors.New("DSH reviewer run completed without a structured completion")
	}
	if !reply.Completion.Valid() {
		return errors.New("DSH reviewer returned an invalid structured completion")
	}
	r.mu.Lock()
	released := w.released
	r.mu.Unlock()
	if released {
		return errors.New("DSH reviewer worker was released before review completion")
	}
	// The HTTP response is the sole completion delivery. A normal DSH final
	// message is intentionally not a verdict, and no retry follows a possibly
	// completed request.
	return complete(*reply.Completion)
}

func (r *Runtime) Release(ctx context.Context, raw review.Worker) error {
	w, err := r.startRelease(ctx, raw)
	if err != nil || w == nil {
		return err
	}
	var reply response
	err = r.call(ctx, request{Action: "release", WorkerID: w.id}, &reply)
	r.finishRelease(w, err)
	return err
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	ids := make([]string, 0, len(r.workers))
	for w := range r.workers {
		if !w.released && !w.busy {
			ids = append(ids, w.id)
		}
		w.released = true
	}
	r.workers = map[*worker]struct{}{}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, id := range ids {
		// Shutdown release is deliberately best-effort. The DSH plugin owns any
		// native cleanup after this process has forgotten its ephemeral workers.
		_ = r.call(ctx, request{Action: "release", WorkerID: id}, &response{})
	}
	return nil
}

func (r *Runtime) beginRun(raw review.Worker) (*worker, error) {
	w, ok := raw.(*worker)
	if !ok {
		return nil, errors.New("foreign DSH reviewer worker")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.workers[w]; !exists {
		return nil, errors.New("DSH reviewer worker was released")
	}
	if r.closed || w.released {
		return nil, errors.New("DSH reviewer worker was released")
	}
	if w.releasing {
		return nil, errors.New("DSH reviewer worker is releasing")
	}
	if w.busy {
		return nil, errors.New("DSH reviewer worker already has an active run")
	}
	w.busy = true
	return w, nil
}

func (r *Runtime) finishRun(w *worker) {
	r.mu.Lock()
	w.busy = false
	r.mu.Unlock()
}

// startRelease returns nil worker only when a concurrent caller is already
// releasing it; that caller's result is observed before returning locally.
func (r *Runtime) startRelease(ctx context.Context, raw review.Worker) (*worker, error) {
	w, ok := raw.(*worker)
	if !ok {
		return nil, errors.New("foreign DSH reviewer worker")
	}
	r.mu.Lock()
	if _, exists := r.workers[w]; !exists || w.released {
		r.mu.Unlock()
		return nil, nil
	}
	if w.busy {
		r.mu.Unlock()
		return nil, errors.New("cannot release DSH reviewer with an active run")
	}
	if w.releasing {
		done := w.release
		r.mu.Unlock()
		select {
		case <-done:
			return nil, w.releaseErr
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w.releasing = true
	w.release = make(chan struct{})
	r.mu.Unlock()
	return w, nil
}

func (r *Runtime) finishRelease(w *worker, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.releasing = false
	w.releaseErr = err
	if err == nil {
		w.released = true
		delete(r.workers, w)
	}
	close(w.release)
}

func (r *Runtime) operationIDLocked(action string) string {
	r.next++
	return fmt.Sprintf("crew-review-%s-%d-%d", action, time.Now().UnixNano(), r.next)
}

func (r *Runtime) call(ctx context.Context, payload request, out *response) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: call DSH reviewer runtime: %v", review.ErrRuntimeUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read DSH reviewer response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var failure response
		_ = json.Unmarshal(data, &failure)
		message := strings.TrimSpace(failure.Error)
		unavailable := resp.StatusCode == http.StatusNotFound || resp.StatusCode >= http.StatusInternalServerError || strings.Contains(message, "reviewer worker is no longer active") || strings.Contains(message, "reviewer runtime is stopped")
		if message != "" {
			if unavailable {
				return fmt.Errorf("%w: DSH reviewer runtime returned HTTP %d: %s", review.ErrRuntimeUnavailable, resp.StatusCode, message)
			}
			return fmt.Errorf("DSH reviewer runtime returned HTTP %d: %s", resp.StatusCode, message)
		}
		if unavailable {
			return fmt.Errorf("%w: DSH reviewer runtime returned HTTP %d", review.ErrRuntimeUnavailable, resp.StatusCode)
		}
		return fmt.Errorf("DSH reviewer runtime returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode DSH reviewer response: %w", err)
	}
	return nil
}
