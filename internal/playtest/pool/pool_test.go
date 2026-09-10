package pool

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"crew-services/internal/playtest/session"
	"github.com/google/uuid"
)

type fake struct {
	mu                        sync.Mutex
	id                        string
	inputs, cancels, acquires int
	launchStarted             chan struct{}
	launchGate                chan struct{}
}

func (f *fake) Acquire(context.Context, int, int, int, int) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id = uuid.NewString()
	f.acquires++
	return map[string]any{"lease_id": f.id}, nil
}
func (f *fake) Observe(context.Context, string) (map[string]any, error) {
	return map[string]any{"path": "original.png"}, nil
}
func (f *fake) Input(context.Context, string, []map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs++
	return map[string]any{"completed_steps": 1}, nil
}
func (f *fake) Cancel(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return map[string]any{"cancel_requested": true}, nil
}
func (f *fake) Release(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id = ""
	return map[string]any{"released": true}, nil
}
func (f *fake) Status(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.id == "" {
		return map[string]any{"lease": nil}, nil
	}
	return map[string]any{"lease": map[string]any{"id": f.id}}, nil
}
func (f *fake) Launch(ctx context.Context, _ string, _ session.Profile) (map[string]any, error) {
	if f.launchStarted != nil {
		close(f.launchStarted)
		select {
		case <-f.launchGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{"launched": true}, nil
}
func setup(t *testing.T, wait time.Duration) (*Pool, []*fake, []string) {
	t.Helper()
	var services []*session.Service
	var backends []*fake
	var dirs []string
	for i := 0; i < 2; i++ {
		b := &fake{}
		dir := t.TempDir()
		s, err := session.New(b, b, []session.Profile{{ID: "game-a"}, {ID: "game-b"}}, dir, filepath.Join("..", "scriptworker", "worker.mjs"))
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, s)
		backends = append(backends, b)
		dirs = append(dirs, dir)
	}
	p, err := New(services, wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.Close(ctx)
	})
	return p, backends, dirs
}
func start(t *testing.T, p *Pool, game string) session.Session {
	t.Helper()
	v, err := p.Command(context.Background(), session.Request{Op: "start", Game: game})
	if err != nil {
		t.Fatal(err)
	}
	return *v.(*session.Session)
}
func TestIndependentSlotsQueueAndRecovery(t *testing.T) {
	p, backends, _ := setup(t, time.Second)
	a := start(t, p, "game-a")
	b := start(t, p, "game-b")
	if a.SlotID == b.SlotID || a.ID == b.ID {
		t.Fatal("shared session ownership")
	}
	_, err := p.Command(context.Background(), session.Request{Op: "input", SessionID: b.ID, Steps: []map[string]any{{"kind": "wait", "ms": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Command(context.Background(), session.Request{Op: "cancel", SessionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if backends[0].inputs != 0 || backends[1].inputs != 1 || backends[1].cancels != 0 {
		t.Fatal("input/cancel crossed slots")
	}
	result := make(chan session.Session, 1)
	failure := make(chan error, 1)
	go func() {
		v, err := p.Command(context.Background(), session.Request{Op: "start", Game: "game-a"})
		if err != nil {
			failure <- err
			return
		}
		result <- *v.(*session.Session)
	}()
	deadline := time.Now().Add(time.Second)
	for p.Activity().(map[string]any)["queued_starts"].(int) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("start not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = p.Command(context.Background(), session.Request{Op: "stop", SessionID: a.ID}); err != nil {
		t.Fatal(err)
	}
	var next session.Session
	select {
	case next = <-result:
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("queue did not wake")
	}
	if next.SlotID != a.SlotID {
		t.Fatal("did not reuse released slot")
	}
	recovered, err := p.Command(context.Background(), session.Request{Op: "recover", SessionID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	fresh := recovered.(*session.Session)
	if fresh.SlotID != b.SlotID || fresh.ID == b.ID || fresh.PreviousSession != b.ID {
		t.Fatal("recovery lost slot identity")
	}
	if backends[0].id != next.ID {
		t.Fatal("recovery touched other slot")
	}
}
func TestCancelledQueueDoesNotAllocate(t *testing.T) {
	p, backends, _ := setup(t, time.Second)
	start(t, p, "game-a")
	start(t, p, "game-b")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Command(ctx, session.Request{Op: "start", Game: "game-a"}); err == nil {
		t.Fatal("expected cancelled queue")
	}
	if backends[0].acquires != 1 || backends[1].acquires != 1 || p.Activity().(map[string]any)["queued_starts"] != 0 {
		t.Fatal("cancelled request leaked reservation")
	}
}
func TestRestartPreservesOwnershipWithoutReplay(t *testing.T) {
	p, backends, dirs := setup(t, time.Second)
	a := start(t, p, "game-a")
	b := start(t, p, "game-b")
	var restored []*session.Service
	for i, dir := range dirs {
		s, err := session.New(backends[i], backends[i], []session.Profile{{ID: "game-a"}, {ID: "game-b"}}, dir, "")
		if err != nil {
			t.Fatal(err)
		}
		restored = append(restored, s)
	}
	next, err := New(restored, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		v, err := next.Command(context.Background(), session.Request{Op: "status", SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		if v.(map[string]any)["session"].(*session.Session).Phase != "interrupted" {
			t.Fatal("replayed saved session")
		}
	}
	if _, err = next.Command(context.Background(), session.Request{Op: "start", Game: "game-a"}); err == nil {
		t.Fatal("allocated interrupted slot")
	}
}
func TestParallelLaunchAndShutdown(t *testing.T) {
	p, backends, _ := setup(t, time.Second)
	backends[0].launchStarted = make(chan struct{})
	backends[0].launchGate = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := p.Command(context.Background(), session.Request{Op: "start", Game: "game-a"})
		done <- err
	}()
	<-backends[0].launchStarted
	b := start(t, p, "game-b")
	if b.SlotID != "slot-2" {
		t.Fatal("second launch blocked on first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown did not cancel launch")
		}
	case <-time.After(time.Second):
		t.Fatal("launch escaped shutdown")
	}
	for _, b := range backends {
		b.mu.Lock()
		id := b.id
		b.mu.Unlock()
		if id != "" {
			t.Fatal("lease leaked at shutdown")
		}
	}
}
