package session

import (
	"context"
	"os"
	"path/filepath"
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
// their records. Evidence they produced (captures, scripts, browser and
// engine artifacts) is not touched. It returns how many it removed.
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
		if err := os.Remove(filepath.Join(s.stateDir, "session-"+id+".json")); err != nil && !os.IsNotExist(err) {
			continue
		}
		delete(s.sessions, id)
		removed++
	}
	return removed
}
