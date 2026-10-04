package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// watchingLauncher is a hosting launcher whose host can be made to exit.
type watchingLauncher struct {
	hostingLauncher
	exited bool
	reason string
	keeps  []bool
}

func (w *watchingLauncher) Launch(ctx context.Context, id string, p Profile) (map[string]any, error) {
	w.keeps = append(w.keeps, LaunchOptionsFrom(ctx).Keep)
	return w.hostingLauncher.Launch(ctx, id, p)
}

func (w *watchingLauncher) HostEnded(context.Context, string, Profile, map[string]any) (bool, string, error) {
	return w.exited, w.reason, nil
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func watchedService(t *testing.T, launcher *watchingLauncher, state string) (*Service, *fakeClock) {
	t.Helper()
	worker, err := filepath.Abs("../scriptworker/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(&fakeBackend{}, launcher, []Profile{{ID: "hosted", Host: &HostSpec{Repo: "/repo"}}}, state, worker)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return clock.now }
	return s, clock
}

func startWatched(t *testing.T, s *Service, keep bool) string {
	t.Helper()
	value, err := s.Command(context.Background(), Request{Op: "start", Game: "hosted", Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	return value.(*Session).ID
}

func TestUnusedSessionExpiresAndReleasesItsBrowserAndHost(t *testing.T) {
	launcher := &watchingLauncher{}
	s, clock := watchedService(t, launcher, t.TempDir())
	id := startWatched(t, s, false)
	clock.advance(29 * time.Minute)
	if reason, err := s.Reap(context.Background(), 30*time.Minute); reason != "" || err != nil {
		t.Fatalf("expired early: %q %v", reason, err)
	}
	clock.advance(time.Minute)
	reason, err := s.Reap(context.Background(), 30*time.Minute)
	if reason != "idle_expired" || err != nil {
		t.Fatalf("Reap = %q %v", reason, err)
	}
	st := s.sessions[id]
	if st.Phase != "stopped" || st.EndReason != "idle_expired" || st.EndedAt == nil || s.current != "" {
		t.Fatalf("session not ended: phase=%s reason=%q current=%q", st.Phase, st.EndReason, s.current)
	}
	if len(launcher.released) != 1 || launcher.released[0] != id {
		t.Fatalf("host not released: %v", launcher.released)
	}
}

func TestAgentCallsKeepASessionButStatusDoesNot(t *testing.T) {
	launcher := &watchingLauncher{}
	s, clock := watchedService(t, launcher, t.TempDir())
	id := startWatched(t, s, false)
	clock.advance(20 * time.Minute)
	if _, err := s.Command(context.Background(), Request{Op: "observe", SessionID: id}); err != nil {
		t.Fatal(err)
	}
	clock.advance(20 * time.Minute)
	if reason, _ := s.Reap(context.Background(), 30*time.Minute); reason != "" {
		t.Fatalf("a session observed 20 minutes ago expired: %q", reason)
	}
	if _, err := s.Command(context.Background(), Request{Op: "status", SessionID: id}); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute)
	if reason, _ := s.Reap(context.Background(), 30*time.Minute); reason != "idle_expired" {
		t.Fatalf("status renewed the session: %q", reason)
	}
}

func TestKeptSessionDoesNotExpireAndRecoverKeepsIt(t *testing.T) {
	launcher := &watchingLauncher{}
	s, clock := watchedService(t, launcher, t.TempDir())
	id := startWatched(t, s, true)
	clock.advance(10 * time.Hour)
	if reason, _ := s.Reap(context.Background(), 30*time.Minute); reason != "" {
		t.Fatalf("kept session expired: %q", reason)
	}
	value, err := s.Command(context.Background(), Request{Op: "recover", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !value.(*Session).Keep || len(launcher.keeps) != 2 || !launcher.keeps[0] || !launcher.keeps[1] {
		t.Fatalf("keep not carried to the host or the recovered session: %+v %v", value, launcher.keeps)
	}
}

func TestZeroIdleTimeoutNeverExpires(t *testing.T) {
	s, clock := watchedService(t, &watchingLauncher{}, t.TempDir())
	startWatched(t, s, false)
	clock.advance(48 * time.Hour)
	if reason, _ := s.Reap(context.Background(), 0); reason != "" {
		t.Fatalf("expired with no idle limit: %q", reason)
	}
}

func TestHostThatEndedEndsItsSessionEvenWhenDegraded(t *testing.T) {
	launcher := &watchingLauncher{reason: "idle-expired"}
	s, _ := watchedService(t, launcher, t.TempDir())
	id := startWatched(t, s, true)
	s.degrade(id, os.ErrDeadlineExceeded)
	if reason, _ := s.Reap(context.Background(), 30*time.Minute); reason != "" {
		t.Fatalf("running host's session ended: %q", reason)
	}
	launcher.exited = true
	reason, err := s.Reap(context.Background(), 30*time.Minute)
	if reason != "host_exited: idle-expired" || err != nil {
		t.Fatalf("Reap = %q %v", reason, err)
	}
	if st := s.sessions[id]; st.Phase != "stopped" || st.EndReason != reason || s.current != "" {
		t.Fatalf("slot held for a dead host: phase=%s reason=%q current=%q", st.Phase, st.EndReason, s.current)
	}
}

func TestRestartResolvesAnInterruptedSessionWhoseHostIsGone(t *testing.T) {
	state := t.TempDir()
	s, _ := watchedService(t, &watchingLauncher{}, state)
	id := startWatched(t, s, false)
	restarted, _ := watchedService(t, &watchingLauncher{exited: true}, state)
	if st := restarted.sessions[id]; st.Phase != "interrupted" || restarted.current != id {
		t.Fatalf("restart did not interrupt the session: %+v", st)
	}
	reason, err := restarted.Reap(context.Background(), 30*time.Minute)
	if !strings.HasPrefix(reason, "host_exited") || err != nil || restarted.current != "" {
		t.Fatalf("interrupted session kept its slot: %q %v current=%q", reason, err, restarted.current)
	}
}

func TestPruneHistoryForgetsOldEndedSessionsOnly(t *testing.T) {
	state := t.TempDir()
	s, clock := watchedService(t, &watchingLauncher{}, state)
	old := startWatched(t, s, false)
	if _, err := s.Stop(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	clock.advance(15 * 24 * time.Hour)
	current := startWatched(t, s, false)
	if removed := s.PruneHistory(clock.now.Add(-14 * 24 * time.Hour)); removed != 1 {
		t.Fatalf("PruneHistory removed %d, want 1", removed)
	}
	if s.OwnsSession(old) || !s.OwnsSession(current) {
		t.Fatalf("wrong sessions pruned: old=%v current=%v", s.OwnsSession(old), s.OwnsSession(current))
	}
	if _, err := os.Stat(filepath.Join(state, "session-"+old+".json")); !os.IsNotExist(err) {
		t.Fatalf("old session record still on disk: %v", err)
	}
}
