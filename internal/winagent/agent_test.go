package winagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDesktop struct {
	mu       sync.Mutex
	nextPID  int
	alive    map[int]bool
	sent     [][]map[string]any
	released int
	front    bool
	sendErr  error
	starting int
	overlap  bool
}

func (f *fakeDesktop) Start(_ string, args []string, _ string, _ []string, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	f.alive[f.nextPID] = true
	return f.nextPID, nil
}
func (f *fakeDesktop) Stop(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pid] = false
	return nil
}
func (f *fakeDesktop) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}
func (f *fakeDesktop) Window(pid int) (uintptr, error)    { return uintptr(pid), nil }
func (f *fakeDesktop) Image(int) string                   { return "rusty.exe" }
func (f *fakeDesktop) CapturePNG(uintptr) ([]byte, error) { return []byte("png"), nil }
func (f *fakeDesktop) Foreground(uintptr) (bool, error)   { return f.front, nil }
func (f *fakeDesktop) Facts() map[string]any              { return map[string]any{} }
func (f *fakeDesktop) Release() error                     { f.released++; return nil }
func (f *fakeDesktop) Send(steps []map[string]any, _ uintptr) (map[string]any, error) {
	f.sent = append(f.sent, steps)
	return map[string]any{"completed_steps": 0}, f.sendErr
}

func testAgent(t *testing.T) (*Agent, *fakeDesktop) {
	t.Helper()
	desktop := &fakeDesktop{alive: map[int]bool{}, front: true}
	agent := New(Config{BindHost: "127.0.0.1", FirstPort: 47910, LastPort: 47919, Rusty: "rusty.exe", Logs: t.TempDir(),
		Product: map[string]Product{"doom": {Repo: "C:/dev/rusty-doom", Project: "game.csproj", Lanes: 3}}}, desktop)
	var active, peak int
	var mu sync.Mutex
	agent.prepare = func(context.Context, string, string, []string, string) error { return nil }
	agent.probe = func(context.Context, string, func() bool) error {
		mu.Lock()
		active++
		peak = max(peak, active)
		desktop.overlap = peak > 1
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}
	return agent, desktop
}

func TestInstancesGetTheirOwnPortsAndLanes(t *testing.T) {
	agent, _ := testAgent(t)
	var wg sync.WaitGroup
	instances := make([]*Instance, 3)
	for i := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			instance, err := agent.StartInstance(context.Background(), "doom", "session")
			if err != nil {
				t.Error(err)
			}
			instances[i] = instance
		}()
	}
	wg.Wait()
	ports := map[int]bool{}
	for _, instance := range instances {
		ports[instance.Port] = true
	}
	lanes := map[string]bool{}
	for _, instance := range instances {
		lanes[instance.Lane] = true
	}
	if len(ports) != 3 || len(lanes) != 3 || !lanes["C:/dev/rusty-doom"] || !lanes["C:/dev/rusty-doom-lane3"] {
		t.Fatalf("ports %v lanes %v", ports, lanes)
	}
	if _, err := agent.StartInstance(context.Background(), "doom", "fourth"); err == nil || !strings.Contains(err.Error(), "lanes") {
		t.Fatalf("a fourth instance shared a lane: %v", err)
	}
	if err := agent.StopInstance(instances[0].ID); err != nil || len(agent.Instances()) != 2 {
		t.Fatalf("stop: %v %v", err, agent.Instances())
	}
}

func TestOneForegroundLeaseAtATime(t *testing.T) {
	agent, desktop := testAgent(t)
	a, _ := agent.StartInstance(context.Background(), "doom", "a")
	b, _ := agent.StartInstance(context.Background(), "doom", "b")
	lease, err := agent.TakeLease("agent-a", a.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.TakeLease("agent-b", b.ID, time.Minute); err == nil || !strings.HasPrefix(err.Error(), "foreground_busy") {
		t.Fatalf("second holder got the foreground: %v", err)
	}
	steps := []map[string]any{{"kind": "hold", "keys": []any{87}, "ms": 100}}
	if receipt, err := agent.LeaseInput(lease.ID, steps); err != nil || receipt["tier"] != "os" || receipt["delivery"] != "sent" {
		t.Fatalf("input: %v %v", receipt, err)
	}
	if err := agent.EndLease(lease.ID); err != nil || desktop.released != 1 {
		t.Fatalf("end lease released %d: %v", desktop.released, err)
	}
	if _, err := agent.TakeLease("agent-b", b.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseLapsesAndFailedInputIsReleasedNotReplayed(t *testing.T) {
	agent, desktop := testAgent(t)
	now := time.Now()
	agent.now = func() time.Time { return now }
	a, _ := agent.StartInstance(context.Background(), "doom", "a")
	lease, _ := agent.TakeLease("agent-a", a.ID, time.Second)
	if _, err := agent.LeaseInput(lease.ID, []map[string]any{{"kind": "wait", "ms": 2000}}); err == nil {
		t.Fatal("a batch outlasting the lease was accepted")
	}
	desktop.sendErr = errors.New("SendInput failed")
	receipt, err := agent.LeaseInput(lease.ID, []map[string]any{{"kind": "hold", "keys": []any{87}, "ms": 10}})
	if err == nil || !strings.HasPrefix(receipt["delivery"].(string), "uncertain") || desktop.released != 1 || len(desktop.sent) != 1 {
		t.Fatalf("failed input: %v %v released %d sent %d", receipt, err, desktop.released, len(desktop.sent))
	}
	now = now.Add(2 * time.Second)
	if _, err := agent.TakeLease("agent-b", a.ID, time.Minute); err != nil || desktop.released != 2 {
		t.Fatalf("lapsed lease not released: %v %d", err, desktop.released)
	}
}

func TestHTTPRefusesABusyForegroundWithConflict(t *testing.T) {
	agent, _ := testAgent(t)
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	post := func(path, body string) *http.Response {
		response, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	var instance Instance
	json.NewDecoder(post("/v1/instances", `{"product":"doom","holder":"s1"}`).Body).Decode(&instance)
	if response := post("/v1/lease", `{"holder":"a","instance":"`+instance.ID+`","ttl_ms":60000}`); response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
	if response := post("/v1/lease", `{"holder":"b","instance":"`+instance.ID+`","ttl_ms":60000}`); response.StatusCode != http.StatusConflict {
		t.Fatal(response.Status)
	}
}

func TestARestartedAgentStopsWhatItsPreviousRunLeft(t *testing.T) {
	agent, desktop := testAgent(t)
	instance, err := agent.StartInstance(context.Background(), "doom", "s1")
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(agent.config, desktop)
	if stopped := restarted.ReapLeftovers(); len(stopped) != 1 || stopped[0] != instance.ID || desktop.Alive(instance.PID) {
		t.Fatalf("leftovers: %v alive %v", stopped, desktop.Alive(instance.PID))
	}
	if stopped := restarted.ReapLeftovers(); len(stopped) != 0 {
		t.Fatalf("reaped twice: %v", stopped)
	}
}
