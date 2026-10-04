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
	upErr   error
	upPID   int
	exited  bool
	runErr  error
	checks  []devserver.StatusOptions
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
	session := devserver.SessionState{Project: o.Project, RepoRoot: o.RepoRoot, Instance: o.Instance, Label: o.Label, Keep: o.Keep, LocalURL: "http://127.0.0.1:37301/", Port: 37301, PID: 4242}
	if f.upErr != nil {
		session.PID = f.upPID
		return devserver.UpResult{Session: session}, f.upErr
	}
	return devserver.UpResult{Started: true, Session: session}, nil
}

func (f *fakeHosts) Stop(_ context.Context, o devserver.StopOptions) (devserver.StopResult, error) {
	f.stops = append(f.stops, o)
	return devserver.StopResult{Stopped: true, Session: devserver.SessionState{Instance: o.Instance}}, f.stopErr
}

func (f *fakeHosts) Running(_ context.Context, o devserver.StatusOptions) (devserver.SessionState, bool, error) {
	f.checks = append(f.checks, o)
	return devserver.SessionState{}, !f.exited, f.runErr
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
	receipt, err := l.ReleaseHost(context.Background(), "session-1", p, launched["host"].(map[string]any))
	if err != nil || receipt["stopped"] != true || hosts.stops[0].Instance != "session-1" {
		t.Fatalf("release: %v %v %+v", receipt, err, hosts.stops)
	}
}

func TestReleaseTargetsTheLaunchedHostAfterManifestChanges(t *testing.T) {
	hosts, inner := &fakeHosts{}, &recordingLauncher{}
	l := testLauncher(hosts, inner)
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom"}}
	launched, err := l.Launch(context.Background(), "s1", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []ManifestReader{
		func(string, string) (string, error) { return "renamed-project", nil },
		func(string, string) (string, error) { return "", errors.New("manifest removed") },
	} {
		l.Manifest = manifest
		if _, err := l.ReleaseHost(context.Background(), "s1", p, launched["host"].(map[string]any)); err != nil {
			t.Fatal(err)
		}
		last := hosts.stops[len(hosts.stops)-1]
		if last.Project != "doom" || last.RepoRoot != "/repo/doom" || last.Instance != "s1" {
			t.Fatalf("release followed the current manifest: %+v", last)
		}
	}
}

func TestFailedStartReportsAStartedHostForRelease(t *testing.T) {
	hosts := &fakeHosts{upErr: errors.New("health timeout"), upPID: 4242}
	l := testLauncher(hosts, &recordingLauncher{})
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom"}}
	launched, err := l.Launch(context.Background(), "s1", p)
	if err == nil || launched["host"] == nil {
		t.Fatalf("started host not reported: %v %v", launched, err)
	}
	hosts.upPID = 0
	if launched, err = l.Launch(context.Background(), "s2", p); err == nil || launched != nil {
		t.Fatalf("unstarted host reported: %v %v", launched, err)
	}
}

func TestUnhostedProfilesPassThrough(t *testing.T) {
	hosts, inner := &fakeHosts{}, &recordingLauncher{}
	l := testLauncher(hosts, inner)
	p := session.Profile{ID: "web", URL: "http://127.0.0.1:3000/"}
	if _, err := l.Launch(context.Background(), "s", p); err != nil {
		t.Fatal(err)
	}
	if receipt, err := l.ReleaseHost(context.Background(), "s", p, nil); receipt["stopped"] != false || err != nil || len(hosts.ups)+len(hosts.stops) != 0 {
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

func TestReleaseFailuresStayVisible(t *testing.T) {
	hosts := &fakeHosts{}
	l := testLauncher(hosts, &recordingLauncher{})
	p := session.Profile{Host: &session.HostSpec{Repo: "/r"}}
	if receipt, err := l.ReleaseHost(context.Background(), "s", p, nil); err != nil || receipt["stopped"] != false || len(hosts.stops) != 0 {
		t.Fatalf("no recorded host: %v %v", receipt, err)
	}
	recorded := map[string]any{"project": "doom", "repo_root": "/r", "instance": "s"}
	for _, stopErr := range []error{devserver.ErrSessionNotFound, errors.New("kill failed")} {
		hosts.stopErr = stopErr
		if _, err := l.ReleaseHost(context.Background(), "s", p, recorded); !errors.Is(err, stopErr) {
			t.Fatalf("stop failure hidden: %v", err)
		}
	}
	if _, err := l.ReleaseHost(context.Background(), "s", p, map[string]any{"project": "doom"}); err == nil {
		t.Fatal("incomplete identity accepted")
	}
}

func TestHostIsLabelledForItsSessionAndKeptOnRequest(t *testing.T) {
	hosts := &fakeHosts{}
	l := testLauncher(hosts, &recordingLauncher{})
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom"}}
	launched, err := l.Launch(session.WithLaunchOptions(context.Background(), session.LaunchOptions{Keep: true}), "s1", p)
	if err != nil {
		t.Fatal(err)
	}
	if hosts.ups[0].Label != "crew-playtest:s1" || !hosts.ups[0].Keep {
		t.Fatalf("host owner: %+v", hosts.ups[0])
	}
	host := launched["host"].(map[string]any)
	if host["label"] != "crew-playtest:s1" || host["keep"] != true {
		t.Fatalf("launch facts lack the owner: %v", host)
	}
	if _, err := l.Launch(context.Background(), "s2", p); err != nil || hosts.ups[1].Keep {
		t.Fatalf("keep leaked into another session: %+v %v", hosts.ups[1], err)
	}
}

func TestHostEndedReadsTheRecordedHostAndExplainsWhy(t *testing.T) {
	hosts := &fakeHosts{}
	l := testLauncher(hosts, &recordingLauncher{})
	var asked []string
	l.Ended = func(_ context.Context, label string) string { asked = append(asked, label); return "idle-expired" }
	p := session.Profile{ID: "doom", Host: &session.HostSpec{Repo: "/repo/doom"}}
	launched, err := l.Launch(context.Background(), "s1", p)
	if err != nil {
		t.Fatal(err)
	}
	host := launched["host"].(map[string]any)
	if ended, _, err := l.HostEnded(context.Background(), "s1", p, host); ended || err != nil {
		t.Fatalf("running host reported ended: %v %v", ended, err)
	}
	if hosts.checks[0].Instance != "s1" || hosts.checks[0].RepoRoot != "/repo/doom" || hosts.checks[0].Project != "doom" {
		t.Fatalf("checked the wrong host: %+v", hosts.checks[0])
	}
	hosts.exited = true
	ended, reason, err := l.HostEnded(context.Background(), "s1", p, host)
	if !ended || reason != "idle-expired" || err != nil || asked[0] != "crew-playtest:s1" {
		t.Fatalf("exited host: %v %q %v asked %v", ended, reason, err, asked)
	}
	l.Ended = func(context.Context, string) string { return "" }
	if _, reason, _ := l.HostEnded(context.Background(), "s1", p, host); reason != "the product host exited" {
		t.Fatalf("crash reason = %q", reason)
	}
	hosts.runErr = devserver.ErrSessionNotFound
	if ended, _, err := l.HostEnded(context.Background(), "s1", p, host); ended || err == nil {
		t.Fatalf("missing host state taken as an exit: %v %v", ended, err)
	}
}

func TestEndedStateReadsTheLastEndedRecordForTheLabel(t *testing.T) {
	listing := []byte(`[
		{"id":"game.s1-10","state":"serving","label":"crew-playtest:s1"},
		{"id":"game.s1-9","state":"port-unavailable","label":"crew-playtest:s1","endedAt":100},
		{"id":"game.s2-8","state":"idle-expired","label":"crew-playtest:s2","endedAt":120},
		{"id":"game.s1-7","state":"idle-expired","label":"crew-playtest:s1","endedAt":130}
	]`)
	if got := endedState(listing, "crew-playtest:s1"); got != "idle-expired" {
		t.Fatalf("endedState = %q", got)
	}
	if got := endedState(listing, "crew-playtest:none"); got != "" {
		t.Fatalf("endedState for an unknown label = %q", got)
	}
	if got := endedState([]byte("RUSTY_ARGUMENT: unknown"), "x"); got != "" {
		t.Fatalf("endedState of non-JSON = %q", got)
	}
}
