package windesk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"crew-services/internal/playtest/session"
)

type fakeAgent struct {
	mu      sync.Mutex
	started []string
	stopped []string
	leases  int
	busy    bool
	// stuck: the instance failed to start and the agent could not stop it.
	stuck bool
	// leasedFor is the instance each lease named ("" for the desktop).
	leasedFor []string
}

func (f *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
		var body struct{ Product, Holder string }
		json.NewDecoder(r.Body).Decode(&body)
		f.started = append(f.started, body.Holder)
		instance := Instance{ID: body.Product + "-1", Port: 48310, Origin: "http://192.168.1.12:48310", PID: 7}
		if f.stuck {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"error": "host exited; stopping it failed", "result": instance})
			return
		}
		json.NewEncoder(w).Encode(instance)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/instances/"):
		if f.stuck {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"error": "taskkill: access denied", "result": map[string]bool{"stopped": false}})
			return
		}
		f.stopped = append(f.stopped, strings.TrimPrefix(r.URL.Path, "/v1/instances/"))
		json.NewEncoder(w).Encode(map[string]bool{"stopped": true})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/desktop.png":
		w.Write([]byte("desktop png"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/lease":
		var body struct{ Instance string }
		json.NewDecoder(r.Body).Decode(&body)
		f.leasedFor = append(f.leasedFor, body.Instance)
		if f.busy {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "foreground_busy: held by other"})
			return
		}
		f.leases++
		json.NewEncoder(w).Encode(map[string]string{"id": "lease-1"})
	case strings.HasSuffix(r.URL.Path, "/input"):
		json.NewEncoder(w).Encode(map[string]any{"delivery": "sent", "tier": "os"})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/lease/"):
		f.leases--
		json.NewEncoder(w).Encode(map[string]bool{"released": true})
	default:
		http.NotFound(w, r)
	}
}

type recorder struct{ profile session.Profile }

func (r *recorder) Launch(_ context.Context, _ string, p session.Profile) (map[string]any, error) {
	r.profile = p
	return map[string]any{"backend": "engine"}, nil
}

func TestWindowsSessionsStartAndStopTheirOwnInstance(t *testing.T) {
	fake := &fakeAgent{}
	server := httptest.NewServer(fake)
	defer server.Close()
	inner := &recorder{}
	l := &Launcher{Inner: inner}
	p := session.Profile{ID: "doom-win", Backend: "engine", Environment: "windows-desktop", Windows: &session.WindowsSpec{Agent: server.URL, Product: "doom"}}
	launched, err := l.Launch(context.Background(), "session-1", p)
	if err != nil {
		t.Fatal(err)
	}
	if inner.profile.URL != "http://192.168.1.12:48310/" || inner.profile.Windows.Instance != "doom-1" || fake.started[0] != "session-1" {
		t.Fatalf("inner launch: %+v started %v", inner.profile, fake.started)
	}
	if launched["session_url"] != "http://192.168.1.12:48310/" || launched["host"].(map[string]any)["windows_instance"] != "doom-1" {
		t.Fatalf("launch facts: %v", launched)
	}
	receipt, err := l.ReleaseHost(context.Background(), "session-1", p, launched["host"].(map[string]any))
	if err != nil || receipt["stopped"] != true || fake.stopped[0] != "doom-1" {
		t.Fatalf("release: %v %v %v", receipt, err, fake.stopped)
	}
}

func TestAFailedStartTheAgentCouldNotStopStaysTheSessions(t *testing.T) {
	fake := &fakeAgent{stuck: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	inner := &recorder{}
	l := &Launcher{Inner: inner}
	p := session.Profile{ID: "doom-win", Backend: "engine", Environment: "windows-desktop", Windows: &session.WindowsSpec{Agent: server.URL, Product: "doom"}}
	launched, err := l.Launch(context.Background(), "session-1", p)
	if err == nil || inner.profile.ID != "" {
		t.Fatalf("a failed start succeeded or connected: %v %+v", err, inner.profile)
	}
	host, _ := launched["host"].(map[string]any)
	if host["windows_instance"] != "doom-1" {
		t.Fatalf("the running instance was not reported: %v", launched)
	}
	if _, err := l.ReleaseHost(context.Background(), "session-1", p, host); err == nil {
		t.Fatal("a failed stop was reported as stopped")
	}
	fake.mu.Lock()
	fake.stuck = false
	fake.mu.Unlock()
	if receipt, err := l.ReleaseHost(context.Background(), "session-1", p, host); err != nil || receipt["stopped"] != true {
		t.Fatalf("retry: %v %v", receipt, err)
	}
}

func TestOtherProfilesPassThrough(t *testing.T) {
	inner := &recorder{}
	l := &Launcher{Inner: inner}
	p := session.Profile{ID: "web", URL: "http://127.0.0.1:3000/"}
	if _, err := l.Launch(context.Background(), "s", p); err != nil || inner.profile.URL != p.URL {
		t.Fatalf("pass-through: %v %+v", err, inner.profile)
	}
	if receipt, err := l.ReleaseHost(context.Background(), "s", p, nil); err != nil || receipt["stopped"] != false {
		t.Fatalf("release without host: %v %v", receipt, err)
	}
}

func TestOSInputEndsItsLeaseAndReportsABusyForeground(t *testing.T) {
	fake := &fakeAgent{}
	server := httptest.NewServer(fake)
	defer server.Close()
	agent := NewAgent(server.URL)
	steps := []map[string]any{{"kind": "hold", "keys": []any{87.0}, "ms": 100.0}}
	receipt, err := agent.OSInput(context.Background(), "crew-playtest-1", "doom-1", steps)
	if err != nil || receipt["delivery"] != "sent" || fake.leases != 0 {
		t.Fatalf("os input: %v %v leases %d", receipt, err, fake.leases)
	}
	fake.busy = true
	receipt, err = agent.OSInput(context.Background(), "crew-playtest-2", "doom-1", steps)
	if err == nil || !strings.HasPrefix(err.Error(), "foreground_busy") || receipt["delivery"] != "not-sent" {
		t.Fatalf("busy foreground: %v %v", receipt, err)
	}
}

func TestTheDesktopCanBeCapturedAndLeased(t *testing.T) {
	fake := &fakeAgent{}
	server := httptest.NewServer(fake)
	defer server.Close()
	agent := NewAgent(server.URL)
	png, err := agent.Desktop(context.Background())
	if err != nil || string(png) != "desktop png" {
		t.Fatalf("desktop: %q %v", png, err)
	}
	steps := []map[string]any{{"kind": "point", "x": 5, "y": 5, "width": 10, "height": 10}, {"kind": "click", "button": 1}}
	if _, err := agent.OSInput(context.Background(), "crew-playtest-1", "", steps); err != nil {
		t.Fatal(err)
	}
	if len(fake.leasedFor) != 1 || fake.leasedFor[0] != "" || fake.leases != 0 {
		t.Fatalf("desktop lease: %v open %d", fake.leasedFor, fake.leases)
	}
}
