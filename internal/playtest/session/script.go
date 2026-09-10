package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"crew-services/internal/playtest/evidence"
)

type scriptJournal interface {
	Append(any) error
	Close() error
}

var openScriptJournal = func(path string) (scriptJournal, error) {
	return evidence.OpenJournal(path)
}

func (s *Service) Run(id, source string, budgetMS int) (any, error) {
	if len(source) == 0 || len(source) > 128*1024 {
		return nil, errors.New("source must contain 1..131072 bytes")
	}
	if budgetMS == 0 {
		budgetMS = 60000
	}
	if budgetMS < 100 || budgetMS > 120000 {
		return nil, errors.New("script budget must be 100..120000 ms, including yields")
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(id); err != nil {
		return nil, err
	}
	scriptID := newID()
	dir := filepath.Join(s.stateDir, "scripts", scriptID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "source.js")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budgetMS)*time.Millisecond)
	a := &activeScript{state: Script{ID: scriptID, SessionID: id, Phase: "running", StartedAt: time.Now().UTC(), SourcePath: path, EventsPath: filepath.Join(dir, "events.jsonl")}, ctx: ctx, cancel: cancel, done: make(chan struct{}), resume: make(chan json.RawMessage, 1)}
	s.scripts[scriptID] = a
	s.sessions[id].ScriptID = scriptID
	if err := s.saveSession(s.sessions[id]); err != nil {
		delete(s.scripts, scriptID)
		cancel()
		return nil, err
	}
	if err := atomicJSON(filepath.Join(dir, "script.json"), a.state); err != nil {
		cancel()
		delete(s.scripts, scriptID)
		return nil, err
	}
	go s.execute(ctx, a, source)
	return clone(a.state), nil
}

func (s *Service) Script(id string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.scripts[id]; a != nil {
		return clone(a.state), nil
	}
	if _, err := parseID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.stateDir, "scripts", id, "script.json"))
	if err != nil {
		return nil, err
	}
	var st Script
	if err = json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Phase == "running" || st.Phase == "yielded" {
		st.Phase = "interrupted"
		st.Error = "service restarted; script was not replayed"
	}
	return st, nil
}

func parseID(id string) (string, error) {
	if len(id) != 36 || strings.ContainsAny(id, "/\\.") {
		return "", errors.New("invalid script id")
	}
	return id, nil
}

func (s *Service) Resume(id string, data json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.require(id)
	if err != nil {
		return nil, err
	}
	a := s.scripts[st.ScriptID]
	if a == nil || a.state.Phase != "yielded" || a.ctx.Err() != nil {
		return nil, errors.New("script is not yielded")
	}
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	select {
	case a.resume <- data:
		a.state.Phase = "running"
		return map[string]any{"resumed": true, "script_id": a.state.ID}, nil
	default:
		return nil, errors.New("resume already pending")
	}
}

type workerCall struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
	Result any             `json:"result"`
	Error  string          `json:"error"`
}

func (s *Service) execute(ctx context.Context, a *activeScript, source string) {
	var failure error
	var returned any
	defer func() {
		contextErr := ctx.Err()
		a.cancel()
		// Independent of Node cooperation; target cancels the active action and its
		// own timer/neutralization owns the remaining safety deadline.
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, cleanupErr := s.backend.Cancel(cleanup, a.state.SessionID)
		cancel()
		if cleanupErr != nil {
			s.degrade(a.state.SessionID, cleanupErr)
			if failure == nil {
				failure = fmt.Errorf("input cleanup uncertain: %w", cleanupErr)
			}
		}
		s.mu.Lock()
		a.state.Phase = "completed"
		if failure != nil {
			a.state.Phase = "failed"
			a.state.Error = failure.Error()
		}
		if contextErr != nil {
			a.state.Phase = "cancelled"
			if errors.Is(contextErr, context.DeadlineExceeded) {
				a.state.Phase = "timed_out"
			}
			a.state.Error = contextErr.Error()
		}
		if cleanupErr != nil {
			a.state.Phase = "cleanup_uncertain"
			a.state.Error = fmt.Sprintf("%v; input cleanup uncertain: %v", failure, cleanupErr)
		}
		now := time.Now().UTC()
		a.state.EndedAt = &now
		a.state.Result = returned
		if err := atomicJSON(filepath.Join(filepath.Dir(a.state.SourcePath), "script.json"), a.state); err != nil {
			a.state.Phase = "failed"
			a.state.Error = "persist final state: " + err.Error()
		}
		s.mu.Unlock()
		close(a.done)
	}()
	journal, err := openScriptJournal(a.state.EventsPath)
	if err != nil {
		failure = err
		return
	}
	defer journal.Close()
	record := func(kind string, value any) error {
		return journal.Append(map[string]any{"at": time.Now().UTC(), "event": kind, "value": value})
	}
	if err = record("program_started", map[string]any{"script_id": a.state.ID, "session_id": a.state.SessionID}); err != nil {
		failure = err
		return
	}
	cmd := exec.CommandContext(ctx, "node", s.worker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		failure = err
		return
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		failure = err
		return
	}
	stderr, err := os.OpenFile(filepath.Join(filepath.Dir(a.state.SourcePath), "worker.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		failure = err
		return
	}
	defer stderr.Close()
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		failure = err
		return
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	encoder := json.NewEncoder(stdin)
	if err = encoder.Encode(map[string]any{"type": "run", "source": source}); err != nil {
		failure = err
		return
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var call workerCall
		if err = json.Unmarshal(scanner.Bytes(), &call); err != nil {
			failure = fmt.Errorf("worker protocol: %w", err)
			return
		}
		if call.Type == "done" {
			returned = call.Result
			if call.Error != "" {
				failure = errors.New(call.Error)
			}
			if recordErr := record("program_done", call); recordErr != nil && failure == nil {
				failure = fmt.Errorf("persist final journal record: %w", recordErr)
			}
			return
		}
		if call.Type != "call" || call.ID == "" {
			failure = errors.New("invalid worker message")
			return
		}
		s.mu.Lock()
		a.state.Calls++
		calls := a.state.Calls
		s.mu.Unlock()
		if calls > 512 {
			failure = errors.New("script exceeded 512 API calls")
			return
		}
		if err = record("call", call); err != nil {
			failure = err
			return
		}
		result, callErr := s.scriptCall(ctx, a, call)
		response := map[string]any{"id": call.ID, "result": result}
		if callErr != nil {
			response["error"] = callErr.Error()
		}
		if err = record("result", response); err != nil {
			failure = err
			return
		}
		if err = encoder.Encode(response); err != nil {
			failure = err
			return
		}
	}
	failure = scanner.Err()
	if failure == nil {
		failure = io.ErrUnexpectedEOF
	}
}

func (s *Service) scriptCall(ctx context.Context, a *activeScript, call workerCall) (any, error) {
	if call.Method == "browser" {
		s.mu.Lock()
		st := s.sessions[a.state.SessionID]
		connected := st != nil && st.Phase == "connected"
		s.mu.Unlock()
		if !connected {
			return nil, errors.New("session is no longer connected; recover before browser actions")
		}
		return s.browserCall(ctx, a.state.SessionID, call.Args)
	}
	if call.Method == "interaction" {
		return s.interactionCall(ctx, a.state.SessionID, call.Args)
	}
	if call.Method == "capture" {
		return s.Capture(ctx, a.state.SessionID, call.Args)
	}
	var args struct {
		State map[string]any   `json:"state"`
		MS    int              `json:"ms"`
		Keys  []string         `json:"keys"`
		Steps []map[string]any `json:"steps"`
		Label string           `json:"label"`
		Data  any              `json:"data"`
		Args  []any            `json:"args"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return nil, err
	}
	if call.Method == "controller" || call.Method == "keyboard" || call.Method == "input" {
		s.mu.Lock()
		st := s.sessions[a.state.SessionID]
		connected := st != nil && st.Phase == "connected"
		s.mu.Unlock()
		if !connected {
			return nil, errors.New("session is no longer connected; recover outside this program before more input")
		}
	}
	switch call.Method {
	case "controller":
		step := args.State
		if step == nil {
			step = map[string]any{}
		}
		step["kind"] = "gamepad"
		step["ms"] = args.MS
		return s.deliver(ctx, a.state.SessionID, []map[string]any{step})
	case "keyboard":
		keys, err := KeyCodes(args.Keys)
		if err != nil {
			return nil, err
		}
		return s.deliver(ctx, a.state.SessionID, []map[string]any{{"kind": "hold", "keys": keys, "ms": args.MS}})
	case "input":
		return s.deliver(ctx, a.state.SessionID, args.Steps)
	case "observe":
		return s.Observe(ctx, a.state.SessionID)
	case "sleep":
		if args.MS < 0 || args.MS > 10000 {
			return nil, errors.New("sleep must be 0..10000 ms")
		}
		return nil, wait(ctx, time.Duration(args.MS)*time.Millisecond)
	case "checkpoint", "yield":
		value := map[string]any{"label": args.Label, "data": args.Data}
		s.mu.Lock()
		a.state.Checkpoint = value
		if call.Method == "yield" {
			a.state.Phase = "yielded"
		}
		err := atomicJSON(filepath.Join(filepath.Dir(a.state.SourcePath), "script.json"), a.state)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if call.Method == "checkpoint" {
			return value, nil
		}
		select {
		case data := <-a.resume:
			var result any
			err := json.Unmarshal(data, &result)
			return result, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "log":
		return args.Args, nil
	default:
		return nil, fmt.Errorf("unknown script method %q", call.Method)
	}
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func KeyCodes(keys []string) ([]int, error) {
	if len(keys) < 1 || len(keys) > 8 {
		return nil, errors.New("provide 1..8 key names")
	}
	named := map[string]int{"ENTER": 13, "ESCAPE": 27, "TAB": 9, "SPACE": 32, "CTRL": 17, "SHIFT": 16, "ALT": 18, "BACKSPACE": 8, "UP": 38, "DOWN": 40, "LEFT": 37, "RIGHT": 39, "F6": 117, "F5": 116}
	codes := make([]int, 0, len(keys))
	for _, key := range keys {
		k := strings.ToUpper(key)
		if len(k) == 1 && ((k[0] >= 'A' && k[0] <= 'Z') || (k[0] >= '0' && k[0] <= '9')) {
			codes = append(codes, int(k[0]))
			continue
		}
		code, ok := named[k]
		if !ok {
			return nil, fmt.Errorf("unsupported key %q", key)
		}
		codes = append(codes, code)
	}
	return codes, nil
}
