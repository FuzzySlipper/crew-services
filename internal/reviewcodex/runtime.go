// Package reviewcodex is the private ephemeral Codex backend for crew-review.
package reviewcodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"crew-services/internal/codexadapter"
	"crew-services/internal/review"
)

type Config struct {
	Command     string
	Args        []string
	Capacity    int
	ProfilePath string
}
type Runtime struct {
	mu       sync.Mutex
	server   codexadapter.EphemeralServer
	capacity int
	profile  string
	workers  map[string]*worker
	reserved int
	next     int
	factory  func(context.Context) (codexadapter.EphemeralServer, error)
}
type worker struct {
	id, threadID   string
	task           review.TaskKey
	released, busy bool
	turnID         string
	complete       func(review.Completion) error
	candidate      *review.Completion
	candidateTurn  string
	callbackErr    error
}

func New(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.Command == "" {
		cfg.Command = "codex"
	}
	if len(cfg.Args) == 0 {
		cfg.Args = []string{"app-server", "--stdio"}
	}
	factory := func(context.Context) (codexadapter.EphemeralServer, error) {
		return codexadapter.StartStdioAppServer(cfg.Command, cfg.Args)
	}
	server, e := factory(ctx)
	if e != nil {
		return nil, e
	}
	r, e := NewWithServer(ctx, server, cfg.Capacity, cfg.ProfilePath)
	if e != nil {
		_ = server.Close()
	}
	if r != nil {
		r.factory = factory
	}
	return r, e
}
func NewWithServer(ctx context.Context, server codexadapter.EphemeralServer, capacity int, profilePath string) (*Runtime, error) {
	if server == nil || capacity < 1 {
		return nil, errors.New("Codex server and positive capacity are required")
	}
	profile, e := os.ReadFile(profilePath)
	if e != nil {
		return nil, fmt.Errorf("read reviewer profile: %w", e)
	}
	r := &Runtime{server: server, capacity: capacity, profile: string(profile) + "\n\nManaged review runtime: use only the controller-bound complete_review tool for a normal review verdict. Do not call Den directly. The controller owns project, task, round, correlation, and reviewer identity.\n", workers: map[string]*worker{}}
	server.SetDynamicToolHandler(r.dynamicTool)
	if e = server.Initialize(ctx); e != nil {
		return nil, e
	}
	return r, nil
}

func completionTool() []map[string]any {
	return []map[string]any{{"type": "function", "name": "complete_review", "description": "Submit the structured managed review result.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string", "enum": []string{"looks_good", "changes_requested"}}, "notes": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"}, "prior_finding_resolutions": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"finding_id": map[string]any{"type": "integer"}, "status": map[string]any{"type": "string"}, "verification_note": map[string]any{"type": "string"}}, "required": []string{"finding_id", "status", "verification_note"}}}, "new_findings": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"category": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}, "notes": map[string]any{"type": "string"}, "file_references": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "test_commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"category", "summary"}}}}, "required": []string{"verdict"}, "additionalProperties": false}}}
}

func (r *Runtime) Acquire(ctx context.Context, task review.TaskKey, _ string, workspace string) (review.Worker, error) {
	r.mu.Lock()
	if len(r.workers)+r.reserved >= r.capacity {
		r.mu.Unlock()
		return nil, errors.New("Codex reviewer pool is at capacity")
	}
	r.next++
	id := fmt.Sprintf("reviewer-%d", r.next)
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
	thread, e := r.server.StartEphemeralThread(ctx, codexadapter.EphemeralThreadOptions{CWD: workspace, DeveloperInstructions: r.profile, DynamicTools: completionTool(), ReadOnly: true})
	if e != nil && r.factory != nil {
		if restartErr := r.restart(ctx); restartErr != nil {
			return nil, fmt.Errorf("Codex App Server lost (%v); restart: %w", e, restartErr)
		}
		thread, e = r.server.StartEphemeralThread(ctx, codexadapter.EphemeralThreadOptions{CWD: workspace, DeveloperInstructions: r.profile, DynamicTools: completionTool(), ReadOnly: true})
	}
	if e != nil {
		return nil, e
	}
	w := &worker{id: id, threadID: thread.ID, task: task}
	r.mu.Lock()
	r.reserved--
	reserved = false
	r.workers[id] = w
	r.mu.Unlock()
	return w, nil
}
func (r *Runtime) restart(ctx context.Context) error {
	r.mu.Lock()
	factory := r.factory
	old := r.server
	for _, w := range r.workers {
		w.released = true
	}
	r.workers = map[string]*worker{}
	r.mu.Unlock()
	if factory == nil {
		return errors.New("Codex App Server is unavailable")
	}
	_ = old.Close()
	next, e := factory(ctx)
	if e != nil {
		return e
	}
	next.SetDynamicToolHandler(r.dynamicTool)
	if e = next.Initialize(ctx); e != nil {
		_ = next.Close()
		return e
	}
	r.mu.Lock()
	r.server = next
	r.mu.Unlock()
	return nil
}
func (r *Runtime) Run(ctx context.Context, raw review.Worker, prompt string, complete func(review.Completion) error) error {
	w, e := r.require(raw)
	if e != nil {
		return e
	}
	r.mu.Lock()
	if w.busy {
		r.mu.Unlock()
		return errors.New("reviewer worker already has an active turn")
	}
	w.busy = true
	w.complete = complete
	w.candidate = nil
	w.candidateTurn = ""
	w.callbackErr = nil
	r.mu.Unlock()
	defer func() { r.mu.Lock(); w.busy = false; w.complete = nil; r.mu.Unlock() }()
	turn, e := r.server.StartEphemeralTurn(ctx, w.threadID, prompt)
	if e != nil {
		return e
	}
	r.mu.Lock()
	w.turnID = turn.ID
	candidate, candidateTurn, callback := w.candidate, w.candidateTurn, w.complete
	r.mu.Unlock()
	if candidate != nil {
		if candidateTurn != turn.ID {
			return errors.New("completion arrived for a stale turn")
		}
		if callbackErr := callback(*candidate); callbackErr != nil {
			r.mu.Lock()
			w.callbackErr = callbackErr
			r.mu.Unlock()
			return callbackErr
		}
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait := make(chan error, 1)
	go func() { _, err := r.server.WaitTurn(waitCtx, w.threadID, turn.ID); wait <- err }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-wait:
			if err != nil {
				return err
			}
			goto completed
		case <-ticker.C:
			for _, interaction := range r.server.Interactions() {
				if interaction.ThreadID == w.threadID && interaction.TurnID == turn.ID {
					_ = r.server.Interrupt(context.Background(), w.threadID, turn.ID)
					cancel()
					<-wait
					return errors.New("noninteractive review cannot satisfy Codex interaction")
				}
			}
		case <-ctx.Done():
			cancel()
			<-wait
			return ctx.Err()
		}
	}
completed:
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.callbackErr != nil {
		return w.callbackErr
	}
	if w.candidate == nil {
		return errors.New("Codex turn completed without complete_review")
	}
	return nil
}
func (r *Runtime) Release(_ context.Context, raw review.Worker) error {
	w, e := r.require(raw)
	if e != nil {
		return e
	}
	r.mu.Lock()
	if w.busy {
		r.mu.Unlock()
		return errors.New("cannot release reviewer with an active turn")
	}
	w.released = true
	delete(r.workers, w.id)
	r.mu.Unlock()
	r.server.ForgetThread(w.threadID)
	return nil
}
func (r *Runtime) Close() error { return r.server.Close() }
func (r *Runtime) require(raw review.Worker) (*worker, error) {
	w, ok := raw.(*worker)
	if !ok {
		return nil, errors.New("foreign reviewer worker")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.released || r.workers[w.id] != w {
		return nil, errors.New("reviewer worker was released")
	}
	return w, nil
}
func (r *Runtime) dynamicTool(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var call struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		CallID    string          `json:"callId"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &call) != nil || call.ThreadID == "" || call.TurnID == "" || call.CallID == "" || call.Tool != "complete_review" {
		return toolResult(false, "invalid managed completion call"), nil
	}
	var candidate review.Completion
	if json.Unmarshal(call.Arguments, &candidate) != nil || !candidate.Valid() {
		return toolResult(false, "invalid completion arguments"), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var w *worker
	for _, x := range r.workers {
		if x.threadID == call.ThreadID {
			w = x
			break
		}
	}
	if w == nil || w.released || !w.busy {
		return toolResult(false, "review worker is no longer active"), nil
	}
	if w.turnID != "" && w.turnID != call.TurnID {
		return toolResult(false, "stale review turn"), nil
	}
	if w.candidate != nil {
		if reflect.DeepEqual(*w.candidate, candidate) {
			if w.callbackErr != nil {
				return toolResult(false, w.callbackErr.Error()), nil
			}
			return toolResult(true, "completion already accepted"), nil
		}
		return toolResult(false, "conflicting second completion"), nil
	}
	w.candidateTurn = call.TurnID
	copy := candidate
	w.candidate = &copy
	if w.turnID == "" {
		return toolResult(true, "completion accepted"), nil
	}
	callback := w.complete
	r.mu.Unlock()
	err := callback(copy)
	r.mu.Lock()
	w.callbackErr = err
	if err != nil {
		return toolResult(false, err.Error()), nil
	}
	return toolResult(true, "completion accepted"), nil
}
func toolResult(ok bool, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"success": ok, "contentItems": []map[string]string{{"type": "inputText", "text": text}}})
	return b
}
