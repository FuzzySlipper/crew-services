package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type hostingLauncher struct {
	launchErr   error
	releaseErrs []error // consumed in order; nil afterwards
	released    []string
	hosts       []map[string]any
}

func (h *hostingLauncher) Launch(_ context.Context, id string, p Profile) (map[string]any, error) {
	return map[string]any{"session_url": "http://127.0.0.1:37301/", "host": map[string]any{"instance": id, "project": "doom", "repo_root": "/repo"}}, h.launchErr
}

func (h *hostingLauncher) ReleaseHost(_ context.Context, id string, p Profile, host map[string]any) (map[string]any, error) {
	h.released = append(h.released, id)
	h.hosts = append(h.hosts, host)
	if len(h.releaseErrs) > 0 {
		err := h.releaseErrs[0]
		h.releaseErrs = h.releaseErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"stopped": true}, nil
}

func hostedService(t *testing.T, launcher *hostingLauncher) *Service {
	t.Helper()
	worker, err := filepath.Abs("../scriptworker/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	profiles := []Profile{{ID: "hosted", Host: &HostSpec{Repo: "/repo"}}}
	s, err := New(&fakeBackend{}, launcher, profiles, t.TempDir(), worker)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHostedSessionUsesItsOwnHostAndReleasesItOnStop(t *testing.T) {
	launcher := &hostingLauncher{}
	s := hostedService(t, launcher)
	value, err := s.Start(context.Background(), "hosted", "")
	if err != nil {
		t.Fatal(err)
	}
	st := value.(*Session)
	if st.Profile.URL != "http://127.0.0.1:37301/" || st.Profile.Host == nil {
		t.Fatalf("session profile should carry its host URL: %+v", st.Profile)
	}
	if registered, _ := s.registry.Profile("hosted"); registered.URL != "" {
		t.Fatalf("registry profile changed: %+v", registered)
	}
	result, err := s.Stop(context.Background(), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := result.(map[string]any)
	if len(launcher.released) != 1 || launcher.released[0] != st.ID || receipt["host"] == nil || launcher.hosts[0]["instance"] != st.ID {
		t.Fatalf("host not released with the session: %v %v %v", launcher.released, receipt, launcher.hosts)
	}
}

func TestFailedHostStopKeepsTheSessionAndRetries(t *testing.T) {
	launcher := &hostingLauncher{releaseErrs: []error{errors.New("kill failed"), errors.New("still failing")}}
	s := hostedService(t, launcher)
	value, err := s.Start(context.Background(), "hosted", "")
	if err != nil {
		t.Fatal(err)
	}
	id := value.(*Session).ID
	result, err := s.Stop(context.Background(), id)
	if err == nil || result.(map[string]any)["host_error"] == nil {
		t.Fatalf("host stop failure hidden: %v %v", result, err)
	}
	if st := s.sessions[id]; st.Phase != "degraded" || s.current != id {
		t.Fatalf("session given up with its host running: phase=%s current=%q", st.Phase, s.current)
	}
	if _, err := s.Command(context.Background(), Request{Op: "recover", SessionID: id}); err == nil {
		t.Fatal("recover started another world while the old host survived")
	}
	if _, err := s.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if st := s.sessions[id]; st.Phase != "stopped" || s.current != "" || len(launcher.released) != 3 {
		t.Fatalf("retry did not release: phase=%s current=%q releases=%d", st.Phase, s.current, len(launcher.released))
	}
}

func TestFailedLaunchWithUnreleasedHostHoldsTheSlot(t *testing.T) {
	launcher := &hostingLauncher{launchErr: errors.New("page failed"), releaseErrs: []error{errors.New("kill failed")}}
	s := hostedService(t, launcher)
	value, err := s.Start(context.Background(), "hosted", "")
	if err == nil {
		t.Fatal("expected launch failure")
	}
	st := value.(*Session)
	if st.Phase != "degraded" || s.current != st.ID || st.Launch["host"] == nil {
		t.Fatalf("failed start abandoned its host: phase=%s current=%q launch=%v", st.Phase, s.current, st.Launch)
	}
	if _, err := s.Stop(context.Background(), st.ID); err != nil || s.current != "" {
		t.Fatalf("stop did not release the recorded host: %v current=%q", err, s.current)
	}
	if got := launcher.hosts[1]["instance"]; got != st.ID {
		t.Fatalf("retry used %v, not the recorded host", launcher.hosts[1])
	}
}

func TestFailedHostedLaunchReleasesHost(t *testing.T) {
	launcher := &hostingLauncher{launchErr: errors.New("page failed")}
	s := hostedService(t, launcher)
	if _, err := s.Start(context.Background(), "hosted", ""); err == nil {
		t.Fatal("expected launch failure")
	}
	if len(launcher.released) != 1 {
		t.Fatalf("failed launch left its host running: %v", launcher.released)
	}
}

func TestHostProfilesAreValidated(t *testing.T) {
	for _, profiles := range [][]Profile{
		{{ID: "a"}},
		{{ID: "a", Host: &HostSpec{}}},
		{{ID: "a", URL: "http://x/", Host: &HostSpec{Repo: "/r"}}},
		{{ID: "a", Host: &HostSpec{Repo: "/r", Path: "play"}}},
	} {
		if err := ValidateProfiles(profiles); err == nil {
			t.Errorf("accepted %+v", profiles[0])
		}
	}
	if err := ValidateProfiles([]Profile{{ID: "a", Host: &HostSpec{Repo: "/r", Path: "/play"}}}); err != nil {
		t.Fatal(err)
	}
}
