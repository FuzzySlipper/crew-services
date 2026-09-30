package session

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
)

// Registry is shared by all slots. Readers receive independent snapshots.
// Reload never changes the profile retained by an existing session.
type Registry struct {
	mu       sync.RWMutex
	profiles []Profile
}

func NewRegistry(profiles []Profile) *Registry {
	return &Registry{profiles: copyProfiles(profiles)}
}

func ValidateProfiles(profiles []Profile) error {
	if len(profiles) == 0 {
		return errors.New("at least one game profile is required")
	}
	seen := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("each game profile requires a nonempty id")
		}
		if p.Host != nil {
			if strings.TrimSpace(p.Host.Repo) == "" {
				return fmt.Errorf("game profile %q host requires repo", p.ID)
			}
			if p.Host.Path != "" && !strings.HasPrefix(p.Host.Path, "/") {
				return fmt.Errorf("game profile %q host path must begin with /", p.ID)
			}
			if strings.TrimSpace(p.URL) != "" {
				return fmt.Errorf("game profile %q sets both url and host; a hosted session's URL is its own host", p.ID)
			}
		} else if strings.TrimSpace(p.URL) == "" {
			return fmt.Errorf("game profile %q requires a url or a host", p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate game profile %q", p.ID)
		}
		seen[p.ID] = true
	}
	return nil
}

func (r *Registry) Replace(profiles []Profile) error {
	if err := ValidateProfiles(profiles); err != nil {
		return err
	}
	next := copyProfiles(profiles)
	r.mu.Lock()
	r.profiles = next
	r.mu.Unlock()
	return nil
}

func (r *Registry) Profiles() []Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return copyProfiles(r.profiles)
}

func (r *Registry) Profile(id string) (Profile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.profiles {
		if p.ID == id {
			return copyProfile(p), nil
		}
	}
	return Profile{}, &UnknownGameError{ID: id}
}

// sessionProfile is called under s.mu, or while restoring before publication.
func (s *Service) sessionProfile(st *Session) (Profile, error) {
	if st.Profile != nil {
		return copyProfile(*st.Profile), nil
	}
	// Compatibility for records written before profile snapshots were introduced.
	return s.registry.Profile(st.Game)
}

func copyProfile(p Profile) Profile {
	p.Controls = maps.Clone(p.Controls)
	if p.Host != nil {
		host := *p.Host
		p.Host = &host
	}
	return p
}
func copyProfiles(profiles []Profile) []Profile {
	result := make([]Profile, len(profiles))
	for i, p := range profiles {
		result[i] = copyProfile(p)
	}
	return result
}

// UnknownGameError lets the file-owning service add a read-only disk diagnostic.
type UnknownGameError struct{ ID string }

func (e *UnknownGameError) Error() string {
	return fmt.Sprintf("unknown game %q: no such profile in the loaded registry; use 'playtest games'; after editing the profile file run 'playtest reload'", e.ID)
}
