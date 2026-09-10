// Package routing selects configured execution adapters. Session policy remains
// in session; adapters retain their own input and observation semantics.
package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"crew-services/internal/playtest/session"
)

type Entry struct {
	Backend  session.Backend
	Launcher session.Launcher
}
type Router struct {
	mu       sync.RWMutex
	Entries  map[string]Entry
	selected string
}

func (r *Router) SelectProfile(p session.Profile) error {
	name := p.Backend
	if name == "" {
		name = "wolf"
	}
	if p.Environment != "" && p.Environment != "service" && !(name == "wolf" && p.Environment == "wolf-target") {
		return fmt.Errorf("unsupported environment %q for backend %q; configure a service on that execution host", p.Environment, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.Entries[name]; !ok || e.Backend == nil || e.Launcher == nil {
		return fmt.Errorf("backend_unavailable: %s is not configured", name)
	}
	if selector, ok := r.Entries[name].Backend.(session.ProfileBackend); ok {
		if err := selector.SelectProfile(p); err != nil {
			return err
		}
	}
	r.selected = name
	return nil
}
func (r *Router) entry() (Entry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name := r.selected
	if name == "" {
		name = "wolf"
		if _, ok := r.Entries[name]; !ok {
			name = "browser"
		}
	}
	e, ok := r.Entries[name]
	if !ok {
		return Entry{}, fmt.Errorf("backend_unavailable: %s", name)
	}
	return e, nil
}
func (r *Router) Acquire(c context.Context, w, h, f, t int) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Acquire(c, w, h, f, t)
}
func (r *Router) Launch(c context.Context, id string, p session.Profile) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Launcher.Launch(c, id, p)
}
func (r *Router) Observe(c context.Context, id string) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Observe(c, id)
}
func (r *Router) Input(c context.Context, id string, v []map[string]any) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Input(c, id, v)
}
func (r *Router) Cancel(c context.Context, id string) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Cancel(c, id)
}
func (r *Router) Status(c context.Context, id string) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Status(c, id)
}
func (r *Router) Release(c context.Context, id string) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	return e.Backend.Release(c, id)
}
func (r *Router) Browser(c context.Context, id string, data json.RawMessage) (map[string]any, error) {
	e, err := r.entry()
	if err != nil {
		return nil, err
	}
	b, ok := e.Backend.(session.BrowserBackend)
	if !ok {
		return nil, fmt.Errorf("capability_unavailable: browser inspection/actions are not exposed by this backend")
	}
	return b.Browser(c, id, data)
}
