package session

import (
	"os"
	"path/filepath"
)

// SlotStatus is an in-process ownership snapshot; it does not renew a lease or
// contact the target. Pool allocation must reserve its slot around Start.
func (s *Service) SlotStatus() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.sessions[s.current])
}
func (s *Service) OwnsSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id] != nil
}
func (s *Service) OwnsScript(id string) bool {
	if _, err := parseID(id); err != nil {
		return false
	}
	s.mu.Lock()
	_, exists := s.scripts[id]
	s.mu.Unlock()
	if exists {
		return true
	}
	_, err := os.Stat(filepath.Join(s.stateDir, "scripts", id, "script.json"))
	return err == nil
}

// ConfigureSlot is called only during service construction, before requests.
func (s *Service) ConfigureSlot(id string, activity func() any) {
	s.slotID = id
	s.poolActivity = activity
	for _, st := range s.sessions {
		st.SlotID = id
	}
}
