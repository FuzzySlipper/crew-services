package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type hostingLauncher struct {
	launchErr error
	released  []string
}

func (h *hostingLauncher) Launch(_ context.Context, id string, p Profile) (map[string]any, error) {
	return map[string]any{"session_url": "http://127.0.0.1:37301/", "host": map[string]any{"instance": id}}, h.launchErr
}

func (h *hostingLauncher) ReleaseHost(_ context.Context, id string, p Profile) (map[string]any, error) {
	h.released = append(h.released, id)
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
	if len(launcher.released) != 1 || launcher.released[0] != st.ID || receipt["host"] == nil {
		t.Fatalf("host not released with the session: %v %v", launcher.released, receipt)
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
