// Package reviewcodex is the private ephemeral Codex backend for crew-review.
package reviewcodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	Model       string
	Effort      string
}
type Runtime struct {
	mu       sync.Mutex
	server   codexadapter.EphemeralServer
	capacity int
	profile  string
	model    string
	effort   string
	workers  map[string]*worker
	reserved int
	next     int
	factory  func(context.Context) (codexadapter.EphemeralServer, error)

	// executable fingerprints the Codex executable on disk. Codex updates
	// itself in place; a running app-server then spawns tool helpers from the
	// new version that it cannot talk to, so the server is restarted once the
	// fingerprint moves away from the one it started with.
	executable func(context.Context) string
	started    string
	// broken asks for a restart once no turn is running, after a turn showed
	// the app-server can no longer run tools or the server itself was lost.
	broken bool
	// restartMu serializes freshness checks and restarts.
	restartMu sync.Mutex
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
	rejectedReason string
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
	executable := func(ctx context.Context) string { return executableFingerprint(ctx, cfg.Command) }
	started := executable(ctx)
	server, e := factory(ctx)
	if e != nil {
		return nil, e
	}
	r, e := newWithServer(ctx, server, cfg.Capacity, cfg.ProfilePath, cfg.Model, cfg.Effort)
	if e != nil {
		_ = server.Close()
	}
	if r != nil {
		r.factory = factory
		r.executable = executable
		r.started = started
	}
	return r, e
}

// executableFingerprint identifies the Codex executable that command names
// now: its resolved path, modification time, size and reported version. It
// returns "" when command cannot be found.
func executableFingerprint(ctx context.Context, command string) string {
	path, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	fingerprint := path
	if info, err := os.Stat(path); err == nil {
		fingerprint += fmt.Sprintf(" %d %d", info.ModTime().UnixNano(), info.Size())
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if version, err := exec.CommandContext(versionCtx, path, "--version").Output(); err == nil {
		fingerprint += " " + strings.TrimSpace(string(version))
	}
	return fingerprint
}

// ensureFresh restarts the app-server before a new review when the Codex
// executable changed since it started, or a turn found it broken. It never
// restarts under a running turn; the caller's job waits in the queue instead.
func (r *Runtime) ensureFresh(ctx context.Context) error {
	if r.factory == nil {
		return nil
	}
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	r.mu.Lock()
	broken := r.broken
	r.mu.Unlock()
	current := ""
	if r.executable != nil {
		current = r.executable(ctx)
	}
	changed := current != "" && current != r.started
	if !broken && !changed {
		return nil
	}
	r.mu.Lock()
	busy := false
	for _, w := range r.workers {
		busy = busy || w.busy
	}
	r.mu.Unlock()
	reason := "a review turn found it unable to run tools"
	if changed {
		reason = "the Codex executable changed on disk"
	}
	if busy {
		return fmt.Errorf("Codex App Server needs a restart because %s, after the running review: %w", reason, review.ErrRuntimeUnavailable)
	}
	if e := r.restartLocked(ctx); e != nil {
		// Keep the job queued: a review cannot run until a server starts.
		return fmt.Errorf("restart Codex App Server because %s: %w: %w", reason, e, review.ErrRuntimeUnavailable)
	}
	r.started = current
	return nil
}
func NewWithServer(ctx context.Context, server codexadapter.EphemeralServer, capacity int, profilePath string) (*Runtime, error) {
	return newWithServer(ctx, server, capacity, profilePath, "", "")
}
func newWithServer(ctx context.Context, server codexadapter.EphemeralServer, capacity int, profilePath, model, effort string) (*Runtime, error) {
	if server == nil || capacity < 1 {
		return nil, errors.New("Codex server and positive capacity are required")
	}
	profile, e := os.ReadFile(profilePath)
	if e != nil {
		return nil, fmt.Errorf("read reviewer profile: %w", e)
	}
	r := &Runtime{server: server, capacity: capacity, profile: string(profile) + "\n\nManaged review runtime: use only the controller-bound complete_review tool for a normal review verdict. Do not call Den directly. The controller owns project, task, round, correlation, and reviewer identity. New findings may use only these categories: blocking_bug, acceptance_gap, test_weakness, or follow_up_candidate. A looks_good verdict must not include new_findings; put non-blocking observations in notes. A changes_requested verdict requires at least one valid new finding for this current review round; a prior-finding resolution with status not_fixed alone is insufficient. Prior-finding resolutions must use one of these terminal statuses: verified_fixed, not_fixed, superseded, or split_to_follow_up.\n", model: model, effort: effort, workers: map[string]*worker{}}
	server.SetDynamicToolHandler(r.dynamicTool)
	if e = server.Initialize(ctx); e != nil {
		return nil, e
	}
	return r, nil
}

func completionTool() []map[string]any {
	return []map[string]any{{"type": "function", "name": "complete_review", "description": "Submit the structured managed review result. A looks_good verdict must not include new findings; put non-blocking observations in notes. A changes_requested verdict requires at least one valid new finding for this current review round; a prior-finding resolution with status not_fixed alone is insufficient.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string", "enum": []string{"looks_good", "changes_requested"}}, "notes": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"}, "prior_finding_resolutions": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"finding_id": map[string]any{"type": "integer"}, "status": map[string]any{"type": "string", "enum": []string{"verified_fixed", "not_fixed", "superseded", "split_to_follow_up"}}, "verification_note": map[string]any{"type": "string"}}, "required": []string{"finding_id", "status", "verification_note"}}}, "new_findings": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"category": map[string]any{"type": "string", "enum": []string{"blocking_bug", "acceptance_gap", "test_weakness", "follow_up_candidate"}}, "summary": map[string]any{"type": "string"}, "notes": map[string]any{"type": "string"}, "file_references": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "test_commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"category", "summary"}}}}, "required": []string{"verdict"}, "additionalProperties": false}}}
}

func (r *Runtime) Acquire(ctx context.Context, task review.TaskKey, _ string, workspace string) (review.Worker, error) {
	r.mu.Lock()
	if len(r.workers)+r.reserved >= r.capacity {
		r.mu.Unlock()
		return nil, review.ErrCapacity
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
	if e := r.ensureFresh(ctx); e != nil {
		return nil, e
	}
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
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	if e := r.restartLocked(ctx); e != nil {
		return e
	}
	if r.executable != nil {
		r.started = r.executable(ctx)
	}
	return nil
}

// restartLocked replaces the app-server; the caller holds restartMu. Every
// worker of the old server is released.
func (r *Runtime) restartLocked(ctx context.Context) error {
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
	r.broken = false
	r.mu.Unlock()
	return nil
}
func (r *Runtime) Run(ctx context.Context, raw review.Worker, prompt string, complete func(review.Completion) error) error {
	if _, e := r.require(raw); e != nil {
		return e
	}
	// A retained worker skips Acquire, so its next turn checks freshness here.
	// A restart releases it, and the job returns to the queue for a new one.
	if e := r.ensureFresh(ctx); e != nil {
		return e
	}
	w, e := r.require(raw)
	if e != nil {
		return e
	}
	r.mu.Lock()
	server := r.server
	if w.busy {
		r.mu.Unlock()
		return errors.New("reviewer worker already has an active turn")
	}
	w.busy = true
	w.complete = complete
	w.turnID = ""
	w.candidate = nil
	w.candidateTurn = ""
	w.callbackErr = nil
	w.rejectedReason = ""
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		w.busy = false
		w.complete = nil
		w.turnID = ""
		r.mu.Unlock()
	}()
	turn, e := server.StartEphemeralTurn(ctx, w.threadID, prompt, codexadapter.EphemeralTurnOptions{Model: r.model, Effort: r.effort})
	if e != nil {
		if ctx.Err() == nil && !codexadapter.IsRPCError(e) {
			return r.infrastructure(fmt.Errorf("start Codex turn: %w", e))
		}
		return e
	}
	r.mu.Lock()
	if w.turnID != "" && w.turnID != turn.ID {
		r.mu.Unlock()
		return errors.New("completion arrived for a stale turn")
	}
	w.turnID = turn.ID
	r.mu.Unlock()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait := make(chan error, 1)
	var done codexadapter.TurnCompletion
	go func() {
		var err error
		done, err = server.WaitTurn(waitCtx, w.threadID, turn.ID)
		wait <- err
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-wait:
			if err != nil {
				if ctx.Err() == nil {
					return r.infrastructure(fmt.Errorf("wait for Codex turn: %w", err))
				}
				return err
			}
			goto completed
		case <-ticker.C:
			for _, interaction := range server.Interactions() {
				if interaction.ThreadID == w.threadID && interaction.TurnID == turn.ID {
					_ = server.Interrupt(context.Background(), w.threadID, turn.ID)
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
		if done.ToolRuntimeError != "" {
			r.broken = true
			return fmt.Errorf("%s; Codex tool runtime failed: %s: %w", missingCompletion(done), done.ToolRuntimeError, review.ErrRuntimeInfrastructure)
		}
		if w.rejectedReason != "" {
			return fmt.Errorf("Codex turn completed without complete_review: %s", w.rejectedReason)
		}
		return errors.New(missingCompletion(done))
	}
	return nil
}

// missingCompletion explains a turn that ended without complete_review using
// what Codex reported: its turn status and error, and the reviewer's last
// message, which usually says why it stopped.
func missingCompletion(done codexadapter.TurnCompletion) string {
	text := "Codex turn completed without complete_review"
	if done.Status != "" && done.Status != "completed" {
		text = "Codex turn " + done.Status + " without complete_review"
	}
	if message := strings.TrimSpace(done.Error); message != "" {
		text += ": " + message
	}
	if message := strings.Join(strings.Fields(done.LastMessage), " "); message != "" {
		const limit = 600
		if len(message) > limit {
			message = strings.ToValidUTF8(message[:limit], "") + "…"
		}
		text += "; reviewer's last message: " + message
	}
	return text
}

// infrastructure marks the app-server for a restart before the next review
// and classifies err as a runtime infrastructure failure.
func (r *Runtime) infrastructure(err error) error {
	r.mu.Lock()
	r.broken = true
	r.mu.Unlock()
	return fmt.Errorf("%w: %w", err, review.ErrRuntimeInfrastructure)
}
func (r *Runtime) Release(_ context.Context, raw review.Worker) error {
	if w, ok := raw.(*worker); ok {
		r.mu.Lock()
		gone := w.released && r.workers[w.id] != w
		r.mu.Unlock()
		if gone {
			// A restart already released it with its app-server.
			return nil
		}
	}
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
	server := r.server
	r.mu.Unlock()
	server.ForgetThread(w.threadID)
	return nil
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	server := r.server
	r.mu.Unlock()
	return server.Close()
}
func (r *Runtime) require(raw review.Worker) (*worker, error) {
	w, ok := raw.(*worker)
	if !ok {
		return nil, errors.New("foreign reviewer worker")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.released || r.workers[w.id] != w {
		return nil, fmt.Errorf("reviewer worker was released: %w", review.ErrRuntimeUnavailable)
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
	invalidArguments := json.Unmarshal(call.Arguments, &candidate) != nil || !candidate.Valid()
	r.mu.Lock()
	defer r.mu.Unlock()
	var w *worker
	for _, x := range r.workers {
		if x.threadID == call.ThreadID {
			w = x
			break
		}
	}
	if invalidArguments {
		if w != nil && !w.released && w.busy {
			w.rejectedReason = "invalid completion arguments"
		}
		return toolResult(false, "invalid completion arguments"), nil
	}
	if w == nil || w.released || !w.busy {
		return toolResult(false, "review worker is no longer active"), nil
	}
	if w.turnID != "" && w.turnID != call.TurnID {
		w.rejectedReason = "stale review turn"
		return toolResult(false, w.rejectedReason), nil
	}
	if w.turnID == "" {
		// Tool calls can arrive before the turn/start response. The call carries
		// the authoritative turn id, allowing validation to happen before the
		// model receives a successful tool result.
		w.turnID = call.TurnID
	}
	if w.candidate != nil {
		if reflect.DeepEqual(*w.candidate, candidate) {
			if w.callbackErr != nil {
				w.rejectedReason = w.callbackErr.Error()
				return toolResult(false, w.rejectedReason), nil
			}
			return toolResult(true, "completion already accepted"), nil
		}
		w.rejectedReason = "conflicting second completion"
		return toolResult(false, w.rejectedReason), nil
	}
	w.candidateTurn = call.TurnID
	copy := candidate
	w.candidate = &copy
	callback := w.complete
	r.mu.Unlock()
	err := callback(copy)
	r.mu.Lock()
	w.callbackErr = err
	if err != nil {
		w.rejectedReason = err.Error()
		// The callback did not persist this completion. Keep the turn open for a
		// corrected complete_review call instead of poisoning the worker.
		w.candidate = nil
		w.candidateTurn = ""
		return toolResult(false, w.rejectedReason), nil
	}
	return toolResult(true, "completion accepted"), nil
}
func toolResult(ok bool, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"success": ok, "contentItems": []map[string]string{{"type": "inputText", "text": text}}})
	return b
}
