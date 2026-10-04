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

func TestPruneEvidenceRemovesOnlyOldEvidenceOfForgottenSessions(t *testing.T) {
	state := t.TempDir()
	s, clock := watchedService(t, &watchingLauncher{}, state)
	known := startWatched(t, s, false)
	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour)
	write := func(path, body string, at time.Time) string {
		t.Helper()
		full := filepath.Join(state, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for p := full; p != state; p = filepath.Dir(p) {
			_ = os.Chtimes(p, at, at)
		}
		return full
	}
	owned := func(id string) string { return `{"session_id":"` + id + `"}` }
	gone, recent := "11111111-0000-0000-0000-000000000001", "11111111-0000-0000-0000-000000000002"
	paths := map[string]bool{
		write("browser/"+gone+"/frame.png", "x", old):            false,
		write("engine/"+gone+"/events.jsonl", "x", old):          false,
		write("receipts/"+gone+"/a.json", "{}", old):             false,
		write("scripts/s-old/script.json", owned(gone), old):     false,
		write("capture-c1.json", owned(gone), old):               false,
		write("interaction-q1.json", owned(gone), old):           false,
		write("presentation-q2.json", owned(gone), old):          false,
		write("browser/"+known+"/frame.png", "x", old):           true,
		write("capture-c2.json", owned(known), old):              true,
		write("browser/"+recent+"/frame.png", "x", now):          true,
		write("capture-c3.json", owned(recent), now):             true,
		write("verification-1/notes.json", owned(gone), old):     true,
		write("local-setup-verification.json", owned(gone), old): true,
		write("capture-unreadable.json", "not json", old):        true,
	}
	// A directory whose own time is old but whose file was appended recently.
	appended := write("engine/"+recent+"/events.jsonl", "x", now)
	_ = os.Chtimes(filepath.Dir(appended), old, old)
	paths[appended] = true

	clock.advance(time.Hour)
	if removed := s.PruneEvidence(now.Add(-14 * 24 * time.Hour)); removed != 7 {
		t.Errorf("PruneEvidence removed %d entries, want 7", removed)
	}
	for path, keep := range paths {
		if _, err := os.Stat(path); (err == nil) != keep {
			t.Errorf("%s kept = %v, want %v", strings.TrimPrefix(path, state), err == nil, keep)
		}
	}
}

func TestRetireToMovesPrunedRecordsAndEvidenceAside(t *testing.T) {
	state, retired := t.TempDir(), t.TempDir()
	s, clock := watchedService(t, &watchingLauncher{}, state)
	s.RetireTo(retired)
	id := startWatched(t, s, false)
	if _, err := s.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	frame := filepath.Join(state, "browser", id, "frame.png")
	if err := os.MkdirAll(filepath.Dir(frame), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(frame, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * 24 * time.Hour)
	_ = os.Chtimes(frame, old, old)
	_ = os.Chtimes(filepath.Dir(frame), old, old)
	clock.advance(15 * 24 * time.Hour)
	cutoff := clock.now.Add(-14 * 24 * time.Hour)
	if s.PruneHistory(cutoff) != 1 || s.PruneEvidence(time.Now().Add(-14*24*time.Hour)) != 1 {
		t.Fatal("nothing pruned")
	}
	day := filepath.Join(retired, clock.now.Format("2006-01-02"))
	for _, moved := range []string{filepath.Join(day, "session-"+id+".json"), filepath.Join(day, "browser", id, "frame.png")} {
		if _, err := os.Stat(moved); err != nil {
			t.Errorf("not moved aside: %v", err)
		}
	}
	if _, err := os.Stat(frame); !os.IsNotExist(err) {
		t.Errorf("evidence still in the state directory: %v", err)
	}
}

func TestCopyTreeKeepsFilesTimesAndLinks(t *testing.T) {
	source, destination := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.MkdirAll(filepath.Join(source, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a", "b", "f.json"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(source, "a", "b", "f.json"), old, old)
	if err := os.Symlink("b/f.json", filepath.Join(source, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "a", "b", "f.json"))
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("copied file: %v %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(destination, "a", "link")); err != nil || link != "b/f.json" {
		t.Fatalf("link: %q %v", link, err)
	}
	if err := moveAside(source, destination); err == nil {
		t.Fatal("moveAside overwrote an existing destination")
	}
}
