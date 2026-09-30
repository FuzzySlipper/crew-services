package hosting

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"crew-services/internal/devserver"
	"crew-services/internal/playtest/session"
)

type fakeHosts struct {
	mu      sync.Mutex
	active  int
	overlap bool
	ups     []devserver.UpOptions
	stops   []devserver.StopOptions
	stopErr error
}

func (f *fakeHosts) Up(_ context.Context, o devserver.UpOptions) (devserver.UpResult, error) {
	f.mu.Lock()
	f.active++
	f.overlap = f.overlap || f.active > 1
	f.ups = append(f.ups, o)
	f.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return devserver.UpResult{Started: true, Session: devserver.SessionState{Project: o.Project, Instance: o.Instance, LocalURL: "http://127.0.0.1:37301/", Port: 37301}}, nil
}

func (f *fakeHosts) Stop(_ context.Context, o devserver.StopOptions) (devserver.StopResult, error) {
	f.stops = append(f.stops, o)
	return devserver.StopResult{Stopped: true, Session: devserver.SessionState{Instance: o.Instance}}, f.stopErr
}

type recordingLauncher struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordingLauncher) Launch(_ context.Context, _ string, p session.Profile) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, p.URL)
	return map[string]any{"backend": "fake"}, nil
}

func testLauncher(hosts *fakeHosts, inner *recordingLauncher) *Launcher {
	return &Launcher{Inner: inner, Hosts: hosts, Locks: &RepoLocks{},
		Manifest: func(string, string) (string, error) { return "doom", nil }}
}

func TestHostedLaunchStartsInstanceAndOpensItsURL(t *testing.T) {
	hosts, inner := &fakeHosts{}, &recordingLauncher{}
	l := testLauncher(hosts, inner)
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom", Path: "/play"}}
	launched, err := l.Launch(context.Background(), "session-1", p)
	if err != nil {
		t.Fatal(err)
	}
	if hosts.ups[0].Instance != "session-1" || hosts.ups[0].Project != "doom" || inner.urls[0] != "http://127.0.0.1:37301/play" {
		t.Fatalf("host/url: %+v %v", hosts.ups, inner.urls)
	}
	if launched["session_url"] != "http://127.0.0.1:37301/play" || launched["backend"] != "fake" || launched["host"] == nil {
		t.Fatalf("launch facts: %v", launched)
	}
	receipt, err := l.ReleaseHost(context.Background(), "session-1", p)
	if err != nil || receipt["stopped"] != true || hosts.stops[0].Instance != "session-1" {
		t.Fatalf("release: %v %v %+v", receipt, err, hosts.stops)
	}
}

func TestUnhostedProfilesPassThrough(t *testing.T) {
	hosts, inner := &fakeHosts{}, &recordingLauncher{}
	l := testLauncher(hosts, inner)
	p := session.Profile{ID: "web", URL: "http://127.0.0.1:3000/"}
	if _, err := l.Launch(context.Background(), "s", p); err != nil {
		t.Fatal(err)
	}
	if receipt, err := l.ReleaseHost(context.Background(), "s", p); receipt != nil || err != nil || len(hosts.ups)+len(hosts.stops) != 0 {
		t.Fatalf("unhosted profile touched hosts: %v %v %+v", receipt, err, hosts)
	}
	if inner.urls[0] != p.URL {
		t.Fatal(inner.urls)
	}
}

func TestSameCheckoutStartsSerially(t *testing.T) {
	hosts, inner := &fakeHosts{}, &recordingLauncher{}
	l := testLauncher(hosts, inner)
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom"}}
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = l.Launch(context.Background(), id, p) }()
	}
	wg.Wait()
	if hosts.overlap || len(hosts.ups) != 3 {
		t.Fatalf("overlap=%v ups=%d", hosts.overlap, len(hosts.ups))
	}
}

func TestMissingHostSessionIsAlreadyReleased(t *testing.T) {
	hosts := &fakeHosts{stopErr: devserver.ErrSessionNotFound}
	l := testLauncher(hosts, &recordingLauncher{})
	receipt, err := l.ReleaseHost(context.Background(), "s", session.Profile{Host: &session.HostSpec{Repo: "/r"}})
	if err != nil || receipt["stopped"] != false {
		t.Fatalf("%v %v", receipt, err)
	}
	hosts.stopErr = errors.New("kill failed")
	if _, err := l.ReleaseHost(context.Background(), "s", session.Profile{Host: &session.HostSpec{Repo: "/r"}}); err == nil {
		t.Fatal("stop failure hidden")
	}
}
