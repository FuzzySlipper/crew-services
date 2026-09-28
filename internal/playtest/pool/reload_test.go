package pool

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/playtest/session"
)

func reloadPool(t *testing.T) (*Pool, *session.Registry) {
	t.Helper()
	profiles := []session.Profile{{ID: "a", URL: "http://localhost/a", Controls: map[string]string{"W": "old"}}}
	registry := session.NewRegistry(profiles)
	root := t.TempDir()
	factory := func(index int) (*session.Service, func(), error) {
		b := &fake{}
		s, err := session.NewWithRegistry(b, b, registry, filepath.Join(root, slotID(index)), "")
		return s, func() {}, err
	}
	first, _, err := factory(0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewResizable([]*session.Service{first}, time.Second, registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(context.Background()) })
	return p, registry
}

func TestReloadGrowthShrinkAndProfileSnapshot(t *testing.T) {
	p, registry := reloadPool(t)
	a := start(t, p, "a")
	profiles := []session.Profile{{ID: "a", URL: "http://localhost/changed", Controls: map[string]string{"W": "new"}}, {ID: "b", URL: "http://localhost/b"}}
	if err := p.Reload(profiles, 3, 0); err != nil {
		t.Fatal(err)
	}
	b := start(t, p, "b")
	c := start(t, p, "a")
	if c.Profile.URL != profiles[0].URL || a.Profile.URL != "http://localhost/a" {
		t.Fatal("profiles not snapshotted")
	}
	if err := p.Reload(profiles[:1], 1, time.Second); err == nil || !strings.Contains(err.Error(), "pool_shrink_busy") {
		t.Fatal(err)
	}
	if len(registry.Profiles()) != 2 || p.Activity().(map[string]any)["capacity"] != 3 {
		t.Fatal("failed shrink changed config")
	}
	for _, st := range []session.Session{a, b, c} {
		if _, err := p.Command(context.Background(), session.Request{Op: "input", SessionID: st.ID, Steps: []map[string]any{{"kind": "wait", "ms": 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []session.Session{b, c} {
		if _, err := p.Command(context.Background(), session.Request{Op: "stop", SessionID: st.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Reload(profiles[:1], 1, 0); err != nil {
		t.Fatal(err)
	}
	// Historical records remain inspectable, but cannot recover beyond capacity.
	if _, err := p.Command(context.Background(), session.Request{Op: "status", SessionID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Command(context.Background(), session.Request{Op: "recover", SessionID: c.ID}); err == nil {
		t.Fatal("recovered disabled slot")
	}
	if err := p.Reload(profiles, 3, 0); err != nil {
		t.Fatal(err)
	}
	if len(p.slots) != 3 {
		t.Fatal("growth did not reuse idle slots")
	}
	d := start(t, p, "b")
	if d.SlotID != b.SlotID {
		t.Fatal(d.SlotID)
	}
}

func TestReloadFailureReleasesStagedSlotsWithoutPublishing(t *testing.T) {
	p, r := reloadPool(t)
	start(t, p, "a")
	original := p.factory
	released := 0
	p.factory = func(i int) (*session.Service, func(), error) {
		if i == 2 {
			return nil, nil, errors.New("unavailable target")
		}
		s, _, err := original(i)
		return s, func() { released++ }, err
	}
	if err := p.Reload([]session.Profile{{ID: "b", URL: "http://localhost/b"}}, 3, 0); err == nil {
		t.Fatal("accepted failed construction")
	}
	if released != 1 || len(p.slots) != 1 || p.capacity != 1 || r.Profiles()[0].ID != "a" || p.wait != time.Second {
		t.Fatal("partial configuration applied")
	}
}

func TestReloadCannotShrinkReservedLaunch(t *testing.T) {
	p, r := reloadPool(t)
	start(t, p, "a")
	b := &fake{launchStarted: make(chan struct{}), launchGate: make(chan struct{})}
	p.factory = func(i int) (*session.Service, func(), error) {
		s, err := session.NewWithRegistry(b, b, r, t.TempDir(), "")
		return s, func() {}, err
	}
	if err := p.Reload(r.Profiles(), 2, time.Second); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.Command(context.Background(), session.Request{Op: "start", Game: "a"})
		done <- err
	}()
	<-b.launchStarted
	if err := p.Reload(r.Profiles(), 1, 0); err == nil {
		t.Fatal("removed reserved slot")
	}
	close(b.launchGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReloadConcurrentDiscoveryAndGrowth(t *testing.T) {
	p, r := reloadPool(t)
	start(t, p, "a")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 30 {
				p.Activity()
				if _, err := p.Command(context.Background(), session.Request{Op: "games"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for i := 1; i < 20; i++ {
		if err := p.Reload(r.Profiles(), 1+i%3, 0); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestReloadWakesQueuedStartWhenGrowing(t *testing.T) {
	p, r := reloadPool(t)
	start(t, p, "a")
	done := make(chan error, 1)
	go func() {
		_, err := p.Command(context.Background(), session.Request{Op: "start", Game: "a"})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for p.Activity().(map[string]any)["queued_starts"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("start never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.Reload(r.Profiles(), 2, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.Activity().(map[string]any)["active_count"] != 2 {
		t.Fatal("queued start did not use new capacity")
	}
}
