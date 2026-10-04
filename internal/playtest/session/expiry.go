package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reap ends the slot's session when its product host has ended by itself, or
// when no agent call has named it for idle (zero never expires). A kept
// session and one running a script do not expire; a host that ended always
// ends its session, whatever the phase, so no slot is held for a dead host.
// It returns the reason it ended the session, or "" when it left it alone.
// Only the pool's reaper calls it; inspection never does.
func (s *Service) Reap(ctx context.Context, idle time.Duration) (string, error) {
	s.mu.Lock()
	id := s.current
	st := s.sessions[id]
	if st == nil || st.Phase == "starting" {
		s.mu.Unlock()
		return "", nil
	}
	profile, profileErr := s.sessionProfile(st)
	host := launchedHost(st.Launch)
	last := st.LastActivity
	if last.IsZero() {
		last = st.CreatedAt
	}
	keep := st.Keep
	scripting := s.scriptRunning(st.ScriptID)
	s.mu.Unlock()

	reason := ""
	if watcher, ok := s.launcher.(HostWatcher); ok && profileErr == nil && host != nil {
		// An unreadable host state is not taken as ended; idle expiry still applies.
		if ended, why, err := watcher.HostEnded(ctx, id, profile, host); err == nil && ended {
			reason = "host_exited"
			if why != "" {
				reason += ": " + why
			}
		}
	}
	if reason == "" && idle > 0 && !keep && !scripting && s.now().Sub(last) >= idle {
		reason = "idle_expired"
	}
	if reason == "" {
		return "", nil
	}
	_, err := s.stop(ctx, id, reason)
	return reason, err
}

// scriptRunning is called under s.mu.
func (s *Service) scriptRunning(id string) bool {
	script := s.scripts[id]
	if script == nil {
		return false
	}
	select {
	case <-script.done:
		return false
	default:
		return true
	}
}

// PruneHistory forgets ended sessions that ended before cutoff and removes
// their records; PruneEvidence then removes what they produced. It returns
// how many it removed.
func (s *Service) PruneHistory(cutoff time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, st := range s.sessions {
		if id == s.current || (st.Phase != "stopped" && st.Phase != "failed") {
			continue
		}
		ended := st.CreatedAt
		if st.EndedAt != nil {
			ended = *st.EndedAt
		}
		if !ended.Before(cutoff) {
			continue
		}
		if err := s.retire(filepath.Join(s.stateDir, "session-"+id+".json")); err != nil && !os.IsNotExist(err) {
			continue
		}
		delete(s.sessions, id)
		removed++
	}
	return removed
}

// evidenceDirs hold one directory per session, named by its ID: the browser
// and engine backends' artifacts and assist receipts.
var evidenceDirs = []string{"browser", "engine", "receipts"}

// evidenceFiles are per-call records in the state directory itself; each
// names its session in session_id.
var evidenceFiles = []string{"capture-", "presentation-", "interaction-"}

// PruneEvidence removes evidence of sessions this slot no longer has a record
// of, once nothing in it changed after cutoff. Because PruneHistory forgets a
// session at the same cutoff, a session's evidence is kept exactly as long as
// its record. It touches only what the service writes: per-session directories
// under browser/, engine/ and receipts/, scripts/<id>/, and capture-,
// presentation- and interaction- records. Anything else in the state
// directory (hand-made verification folders, other slots) is left alone. It
// returns how many entries it removed.
func (s *Service) PruneEvidence(cutoff time.Time) int {
	s.mu.Lock()
	known := make(map[string]bool, len(s.sessions))
	for id := range s.sessions {
		known[id] = true
	}
	s.mu.Unlock()
	removed := 0
	for _, kind := range evidenceDirs {
		entries, _ := os.ReadDir(filepath.Join(s.stateDir, kind))
		for _, entry := range entries {
			path := filepath.Join(s.stateDir, kind, entry.Name())
			if entry.IsDir() && !known[entry.Name()] && unchangedSince(path, cutoff) && s.retire(path) == nil {
				removed++
			}
		}
	}
	scripts, _ := os.ReadDir(filepath.Join(s.stateDir, "scripts"))
	for _, entry := range scripts {
		path := filepath.Join(s.stateDir, "scripts", entry.Name())
		if !entry.IsDir() || !unchangedSince(path, cutoff) {
			continue
		}
		if owner, ok := recordSession(filepath.Join(path, "script.json")); ok && !known[owner] && s.retire(path) == nil {
			removed++
		}
	}
	files, _ := os.ReadDir(s.stateDir)
	for _, entry := range files {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || !hasAnyPrefix(name, evidenceFiles) {
			continue
		}
		path := filepath.Join(s.stateDir, name)
		if !unchangedSince(path, cutoff) {
			continue
		}
		if owner, ok := recordSession(path); ok && !known[owner] && s.retire(path) == nil {
			removed++
		}
	}
	return removed
}

// RetireTo makes pruning move what it removes into dir/<date>/<path below
// the state directory> instead of deleting it, so a pruning round can be
// checked before anything is lost. An empty dir deletes. It is called only
// during service construction, before requests.
func (s *Service) RetireTo(dir string) { s.retireDir = dir }

// retire removes a pruned record or directory, or moves it aside.
func (s *Service) retire(path string) error {
	if s.retireDir == "" {
		return os.RemoveAll(path)
	}
	relative, err := filepath.Rel(s.stateDir, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return fmt.Errorf("%s is outside the state directory", path)
	}
	return moveAside(path, filepath.Join(s.retireDir, s.now().Format("2006-01-02"), relative))
}

// unchangedSince is true when nothing at or below path was modified after
// cutoff. A directory's own time does not change when a file in it is
// appended to, so every entry counts.
func unchangedSince(path string, cutoff time.Time) bool {
	unchanged := true
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			unchanged = false
			return fs.SkipAll
		}
		return nil
	})
	return err == nil && unchanged
}

// recordSession reads the session a JSON evidence record belongs to. A record
// that cannot be read or names no session is kept.
func recordSession(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var record struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(data, &record) != nil || record.SessionID == "" {
		return "", false
	}
	return record.SessionID, true
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
