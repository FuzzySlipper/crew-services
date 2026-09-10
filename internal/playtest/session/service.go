package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"crew-services/internal/playtest/evidence"
	"crew-services/internal/playtest/target"
	"github.com/google/uuid"
)

type activeScript struct {
	state  Script
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	resume chan json.RawMessage
}

type Service struct {
	slotID           string
	poolActivity     func() any
	mu               sync.Mutex
	opMu             sync.Mutex
	backend          Backend
	launcher         Launcher
	profiles         []Profile
	stateDir, worker string
	sessions         map[string]*Session
	scripts          map[string]*activeScript
	current          string
	launchCancel     context.CancelFunc
}

func New(backend Backend, launcher Launcher, profiles []Profile, stateDir, worker string) (*Service, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	s := &Service{backend: backend, launcher: launcher, profiles: profiles, stateDir: stateDir, worker: worker, sessions: map[string]*Session{}, scripts: map[string]*activeScript{}}
	files, err := filepath.Glob(filepath.Join(stateDir, "session-*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var saved Session
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
		if saved.Phase != "stopped" && saved.Phase != "failed" {
			saved.Phase = "interrupted"
			saved.LastError = "Session service restarted; execution was not replayed. Stop or recover explicitly."
			if s.current != "" {
				return nil, errors.New("multiple unfinished saved sessions require reconciliation")
			}
			s.current = saved.ID
		}
		s.sessions[saved.ID] = &saved
		if s.current == saved.ID {
			if selector, ok := backend.(ProfileBackend); ok {
				p, err := s.profile(saved.Game)
				if err != nil {
					return nil, err
				}
				if err := selector.SelectProfile(p); err != nil {
					return nil, err
				}
			}
		}
		if err := s.saveSession(&saved); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func atomicJSON(path string, value any) error {
	return evidence.WriteJSONFile(path, value)
}

func (s *Service) saveSession(st *Session) error {
	return atomicJSON(filepath.Join(s.stateDir, "session-"+st.ID+".json"), st)
}

func clone[T any](value T) T {
	data, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(data, &result)
	return result
}

func (s *Service) profile(id string) (Profile, error) {
	for _, p := range s.profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unknown game %q; use games", id)
}

func (s *Service) require(id string) (*Session, error) {
	st := s.sessions[id]
	if st == nil {
		return nil, errors.New("unknown session")
	}
	return st, nil
}

func (s *Service) Command(ctx context.Context, r Request) (any, error) {
	switch r.Op {
	case "games":
		return clone(s.profiles), nil
	case "game":
		return s.profile(r.Game)
	case "start":
		return s.Start(ctx, r.Game, "")
	case "status":
		return s.Status(ctx, r.SessionID)
	case "observe":
		return s.Observe(ctx, r.SessionID)
	case "browser":
		return s.Browser(ctx, r.SessionID, r.Data)
	case "interaction":
		return s.Interaction(ctx, r.SessionID, r.Data)
	case "capture":
		return s.Capture(ctx, r.SessionID, r.Data)
	case "input":
		return s.ManualInput(ctx, r.SessionID, r.Steps)
	case "run":
		return s.Run(r.SessionID, r.Source, r.BudgetMS)
	case "script":
		return s.Script(r.ScriptID)
	case "cancel":
		return s.Cancel(ctx, r.SessionID)
	case "resume":
		return s.Resume(r.SessionID, r.Data)
	case "stop":
		return s.Stop(ctx, r.SessionID)
	case "recover":
		s.mu.Lock()
		st, err := s.require(r.SessionID)
		game := ""
		if err == nil {
			game = st.Game
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if _, err = s.Stop(ctx, r.SessionID); err != nil {
			return nil, err
		}
		return s.Start(ctx, game, r.SessionID)
	default:
		return nil, errors.New("unknown operation")
	}
}

func (s *Service) Start(ctx context.Context, game, previous string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	p, err := s.profile(game)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	busy := s.current != ""
	s.mu.Unlock()
	if busy {
		return nil, errors.New("single target slot occupied; stop the current session first")
	}
	if selector, ok := s.backend.(ProfileBackend); ok {
		if err := selector.SelectProfile(p); err != nil {
			return nil, err
		}
	}
	lease, err := s.backend.Acquire(ctx, 1280, 720, 30, 300)
	if err != nil {
		return nil, err
	}
	id, _ := lease["lease_id"].(string)
	if id == "" {
		return nil, errors.New("backend returned no lease_id")
	}
	st := &Session{SlotID: s.slotID, ID: id, Game: game, Phase: "starting", CreatedAt: time.Now().UTC(), PreviousSession: previous}
	s.mu.Lock()
	s.sessions[id] = st
	s.current = id
	s.launchCancel = cancel
	err = s.saveSession(st)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.launchCancel = nil; s.mu.Unlock() }()
	var launched map[string]any
	if err == nil {
		launched, err = s.launcher.Launch(ctx, id, p)
	}
	if err != nil {
		captureCtx, captureCancel := context.WithTimeout(context.Background(), 5*time.Second)
		failureFrame, _ := s.backend.Observe(captureCtx, id)
		captureCancel()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		receipt, cleanupErr := s.backend.Release(cleanupCtx, id)
		s.mu.Lock()
		st.Phase = "failed"
		st.LastError = err.Error()
		st.Launch = map[string]any{"failure_observation": failureFrame}
		if cleanupErr == nil && receipt["released"] == true {
			s.current = ""
		} else {
			st.Phase = "degraded"
			st.LastError += fmt.Sprintf("; cleanup unresolved: %v %v", cleanupErr, receipt)
		}
		_ = s.saveSession(st)
		result := clone(st)
		s.mu.Unlock()
		return result, err
	}
	s.mu.Lock()
	st.Phase = "connected"
	st.Launch = launched
	err = s.saveSession(st)
	result := clone(st)
	s.mu.Unlock()
	return result, err
}

func (s *Service) Status(ctx context.Context, id string) (any, error) {
	s.mu.Lock()
	if id == "" {
		id = s.current
	}
	st := clone(s.sessions[id])
	s.mu.Unlock()
	// Read-only listing does not renew target expiry.
	target, err := s.backend.Status(ctx, "")
	if err != nil {
		return map[string]any{"session": st, "target_error": err.Error()}, nil
	}
	if st != nil && st.Phase == "connected" {
		lease, _ := target["lease"].(map[string]any)
		if lease == nil || lease["id"] != st.ID || lease["state"] == "unavailable" {
			s.degrade(st.ID, errors.New("target lease ended; recover explicitly to start a new game session"))
			s.mu.Lock()
			st = clone(s.sessions[id])
			s.mu.Unlock()
		}
	}
	return map[string]any{"session": st, "target": target}, nil
}

func (s *Service) Observe(ctx context.Context, id string) (any, error) {
	s.mu.Lock()
	_, err := s.require(id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, err := s.backend.Observe(ctx, id)
	if err != nil {
		s.degrade(id, err)
	}
	return result, err
}

func (s *Service) degrade(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.sessions[id]; st != nil && st.Phase != "stopped" {
		st.Phase = "degraded"
		st.LastError = err.Error()
		_ = s.saveSession(st)
	}
}

func (s *Service) available(id string) error {
	st, err := s.require(id)
	if err != nil {
		return err
	}
	if s.current != id || st.Phase != "connected" {
		return fmt.Errorf("session is %s; observe and recover before more input", st.Phase)
	}
	if script := s.scripts[st.ScriptID]; script != nil {
		select {
		case <-script.done:
		default:
			return errors.New("script active; cancel or resume it first")
		}
	}
	return nil
}

func (s *Service) ManualInput(ctx context.Context, id string, steps []map[string]any) (any, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	err := s.available(id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.deliver(ctx, id, steps)
}

func (s *Service) deliver(ctx context.Context, id string, steps []map[string]any) (map[string]any, error) {
	if err := target.ValidateBatch(steps); err != nil {
		return nil, err
	}
	result, err := s.backend.Input(ctx, id, steps)
	if err == nil {
		if e, ok := result["error"].(string); ok && e != "" {
			err = errors.New(e)
		}
		if e, ok := result["evidence_error"].(string); ok && e != "" {
			err = fmt.Errorf("target evidence persistence failed: %s", e)
		}
		if errs, ok := result["release_errors"].([]any); ok && len(errs) > 0 {
			err = fmt.Errorf("input release errors: %v", errs)
		}
	}
	if err != nil && (ctx.Err() == nil || !errors.Is(err, ctx.Err())) {
		s.degrade(id, err)
	}
	return result, err
}

func (s *Service) Cancel(ctx context.Context, id string) (any, error) {
	s.mu.Lock()
	st, err := s.require(id)
	var script *activeScript
	if err == nil {
		if st.Phase == "starting" && s.launchCancel != nil {
			s.launchCancel()
		}
		script = s.scripts[st.ScriptID]
		if script != nil {
			select {
			case <-script.done:
			default:
				script.state.Phase = "cancelling"
				script.cancel()
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, cancelErr := s.backend.Cancel(ctx, id)
	if result["requires_recover"] == true {
		s.degrade(id, errors.New("browser operation terminated; recover explicitly before further input"))
	}
	if script != nil {
		select {
		case <-script.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, cancelErr
}

func (s *Service) Stop(ctx context.Context, id string) (any, error) {
	// Cancel before acquiring opMu: an in-flight manual input owns that mutex.
	_, _ = s.Cancel(ctx, id)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	st, err := s.require(id)
	if err == nil && st.Phase == "stopped" {
		result := clone(st)
		s.mu.Unlock()
		return result, nil
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, err := s.backend.Release(ctx, id)
	if result == nil {
		result = map[string]any{"released": false}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if result["released"] == true {
		st.Phase = "stopped"
		if s.current == id {
			s.current = ""
		}
		evidenceErr, _ := result["evidence_error"].(string)
		if err != nil && evidenceErr == "" {
			evidenceErr = err.Error()
		}
		if evidenceErr != "" {
			st.LastError = "cleanup completed; evidence incomplete: " + evidenceErr
		} else {
			st.LastError = ""
		}
	} else {
		st.Phase = "degraded"
		st.LastError = fmt.Sprintf("cleanup unresolved: %v %v", err, result)
		if err == nil {
			err = errors.New(st.LastError)
		}
	}
	if saveErr := s.saveSession(st); saveErr != nil {
		evidenceErr := "persist session state: " + saveErr.Error()
		if prior, _ := result["evidence_error"].(string); prior != "" {
			result["evidence_error"] = prior + "; " + evidenceErr
		} else {
			result["evidence_error"] = evidenceErr
		}
		if st.Phase == "stopped" {
			st.LastError = "cleanup completed; evidence incomplete: " + result["evidence_error"].(string)
		}
		if err == nil {
			err = saveErr
		}
	}
	return result, err
}

func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	id := s.current
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	_, err := s.Stop(ctx, id)
	return err
}

func newID() string { return uuid.NewString() }
