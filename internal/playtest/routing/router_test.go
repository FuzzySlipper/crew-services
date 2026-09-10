package routing

import (
	"context"
	"crew-services/internal/playtest/session"
	"testing"
)

type fake struct {
	session.Backend
	session.Launcher
	name     string
	selected bool
}

func (f *fake) SelectProfile(session.Profile) error { f.selected = true; return nil }
func (f *fake) Acquire(context.Context, int, int, int, int) (map[string]any, error) {
	return map[string]any{"lease_id": f.name}, nil
}
func TestProfileRoutingDoesNotFallback(t *testing.T) {
	w, b := &fake{name: "wolf"}, &fake{name: "browser"}
	r := &Router{Entries: map[string]Entry{"wolf": {w, w}, "browser": {b, b}}}
	if err := r.SelectProfile(session.Profile{ID: "legacy"}); err != nil {
		t.Fatal(err)
	}
	a, err := r.Acquire(context.Background(), 1, 1, 1, 1)
	if err != nil || a["lease_id"] != "wolf" {
		t.Fatal(a, err)
	}
	if err = r.SelectProfile(session.Profile{Backend: "browser", Environment: "service"}); err != nil {
		t.Fatal(err)
	}
	a, err = r.Acquire(context.Background(), 1, 1, 1, 1)
	if err != nil || a["lease_id"] != "browser" || !b.selected {
		t.Fatal(a, err)
	}
	if err = r.SelectProfile(session.Profile{Backend: "browser", Environment: "windows"}); err == nil {
		t.Fatal("silently changed machine")
	}
	if err = r.SelectProfile(session.Profile{Backend: "missing"}); err == nil {
		t.Fatal("silently changed backend")
	}
	if _, err = r.Browser(context.Background(), "id", nil); err == nil {
		t.Fatal("invented browser capability")
	}
}
