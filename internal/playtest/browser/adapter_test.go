package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/playtest/session"
)

func testAdapter(t *testing.T, endpoint string) (*Adapter, string) {
	t.Helper()
	worker, err := filepath.Abs("worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	chromium := "/usr/bin/chromium"
	if _, err := os.Stat(chromium); err != nil {
		t.Skipf("system Chromium is unavailable: %v", err)
	}
	adapter, err := New(Config{State: t.TempDir(), Worker: worker, Chromium: chromium})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.SelectProfile(session.Profile{ID: "fixture", Backend: "browser", Environment: "service", URL: endpoint}); err != nil {
		t.Fatal(err)
	}
	lease, err := adapter.Acquire(context.Background(), 1280, 720, 30, 300)
	if err != nil {
		t.Fatal(err)
	}
	leaseID, _ := lease["lease_id"].(string)
	if leaseID == "" {
		t.Fatalf("acquire did not return a lease ID: %#v", lease)
	}
	if _, err := adapter.Launch(context.Background(), leaseID, session.Profile{ID: "fixture", Backend: "browser", Environment: "service", URL: endpoint}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = adapter.Release(context.Background(), leaseID) })
	return adapter, leaseID
}

func browserRequest(t *testing.T, adapter *Adapter, leaseID string, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Browser(context.Background(), leaseID, raw)
	if err != nil {
		t.Fatalf("browser %#v: %v", value, err)
	}
	return result
}

func TestHeadlessFixtureDOMInputEvidenceAndNearAssist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>Fixture</title><style>#add{margin:40px;width:120px;height:40px}#cover{position:absolute;left:300px;top:40px;width:120px;height:40px;background:#fff;opacity:.8}</style><input id="name"><button id="add" onclick="document.querySelector('#result').textContent=document.querySelector('#name').value">Add</button><button id="covered" style="position:absolute;left:300px;top:40px;width:120px;height:40px">Covered</button><div id="cover"></div><button id="near" onclick="document.querySelector('#result').textContent='near clicked'">Near</button><div id="result"></div><canvas width="640" height="360" style="width:320px;height:180px"></canvas>`))
	}))
	defer server.Close()
	adapter, leaseID := testAdapter(t, server.URL)

	browserRequest(t, adapter, leaseID, map[string]any{"op": "fill", "selector": "#name", "value": "crew"})
	browserRequest(t, adapter, leaseID, map[string]any{"op": "click", "selector": "#add"})
	inspected := browserRequest(t, adapter, leaseID, map[string]any{"op": "inspect", "selector": "#result"})
	if inspected["title"] != "Fixture" || inspected["renderer"] != "unknown" {
		t.Fatalf("inspect = %#v", inspected)
	}
	near := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 10, "y": 55, "max_distance": 256})
	if near["outcome"] != "ok" || near["token"] == "" {
		t.Fatalf("near = %#v", near)
	}
	selected := browserRequest(t, adapter, leaseID, map[string]any{"op": "select", "token": near["token"], "action": "click"})
	if selected["outcome"] != "ok" || selected["assistance"] != "near_cursor_dom" {
		t.Fatalf("select = %#v", selected)
	}
	covered := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 350, "y": 60, "max_distance": 50})
	if covered["outcome"] != "obstructed" {
		t.Fatalf("covered near = %#v", covered)
	}
	observation, err := adapter.Observe(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if observation["width"] != 1280 || observation["height"] != 720 || observation["path"] == "" || observation["capture_started_at_ns"] == nil || observation["frame_freshness"] != "not measured" {
		t.Fatalf("observation = %#v", observation)
	}
	if _, err := os.Stat(observation["path"].(string)); err != nil {
		t.Fatalf("original screenshot: %v", err)
	}
	journal, err := os.ReadFile(observation["events_path"].(string))
	if err != nil || !strings.Contains(string(journal), "action_requested") || !strings.Contains(string(journal), "near_cursor_dom") {
		t.Fatalf("manual action evidence missing: %s %v", journal, err)
	}
	if observation["dom_assistance"] == nil {
		t.Fatalf("observation omitted assistance facts: %#v", observation)
	}
}

func TestCancellationKillsActiveBrowserCallAndLeavesLeaseUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("<!doctype html><title>slow</title>"))
	}))
	defer server.Close()
	adapter, leaseID := testAdapter(t, server.URL)
	finished := make(chan error, 1)
	go func() {
		_, err := adapter.Input(context.Background(), leaseID, []map[string]any{{"kind": "wait", "ms": 10_000}})
		finished <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		adapter.mu.Lock()
		active := adapter.active
		adapter.mu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("browser input did not become active")
		}
		time.Sleep(10 * time.Millisecond)
	}
	receipt, err := adapter.Cancel(context.Background(), leaseID)
	if err != nil || receipt["requires_recover"] != true {
		t.Fatalf("cancel = %#v, %v", receipt, err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("in-flight call succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call continued after cancellation")
	}
	status, err := adapter.Status(context.Background(), leaseID)
	if err != nil || status["lease"].(map[string]any)["state"] != "unavailable" {
		t.Fatalf("status = %#v, %v", status, err)
	}
}

func TestNearBoundsScrollDisabledStaleAndDefaultMove(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`<!doctype html><title>near fixture</title><style>button{position:absolute;left:40px;width:120px;height:32px}#same{top:40px}#replace{top:90px}#disabled{top:140px}#far{top:1400px}</style><button id="same" onclick="document.title='same-clicked'">Same</button><button id="replace" onclick="const n=document.createElement('button');n.id='same';n.textContent='Same';document.querySelector('#same').replaceWith(n)">Replace</button><button id="disabled" disabled>Disabled</button><button id="far">Far</button>`))
	}))
	defer server.Close()
	adapter, leaseID := testAdapter(t, server.URL)

	first := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 50, "y": 50, "max_distance": 20})
	if first["outcome"] != "ok" {
		t.Fatalf("first near = %#v", first)
	}
	defaultSelect := browserRequest(t, adapter, leaseID, map[string]any{"op": "select", "token": first["token"]})
	if defaultSelect["action"] != "move" {
		t.Fatalf("default select clicked instead of moving: %#v", defaultSelect)
	}
	if inspect := browserRequest(t, adapter, leaseID, map[string]any{"op": "inspect"}); inspect["title"] != "near fixture" {
		t.Fatalf("default select mutated the page: %#v", inspect)
	}

	stale := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 50, "y": 50, "max_distance": 20})
	browserRequest(t, adapter, leaseID, map[string]any{"op": "click", "selector": "#replace"})
	if selected := browserRequest(t, adapter, leaseID, map[string]any{"op": "select", "token": stale["token"], "action": "click"}); selected["outcome"] != "stale" {
		t.Fatalf("replacement with the same descriptor was accepted: %#v", selected)
	}
	if disabled := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 50, "y": 150, "max_distance": 20}); disabled["outcome"] != "disabled" {
		t.Fatalf("disabled target = %#v", disabled)
	}
	if none := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": 1200, "y": 650, "max_distance": 1}); none["outcome"] != "no_candidate" {
		t.Fatalf("no-candidate request = %#v", none)
	}

	browserRequest(t, adapter, leaseID, map[string]any{"op": "click", "selector": "#far"})
	inspected := browserRequest(t, adapter, leaseID, map[string]any{"op": "inspect", "selector": "#far"})
	targets := inspected["targets"].([]any)
	rect := targets[0].(map[string]any)["rect"].(map[string]any)
	scrolled := browserRequest(t, adapter, leaseID, map[string]any{"op": "near", "x": rect["x"].(float64) + 5, "y": rect["y"].(float64) + 5, "max_distance": 10})
	if scrolled["outcome"] != "ok" {
		t.Fatalf("scrolled target = %#v", scrolled)
	}
}

func TestLaunchHTTPFailureAndAcquireFailureDoNotLeaveLeaseOwned(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	worker, err := filepath.Abs("worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{State: t.TempDir(), Worker: worker, Chromium: "/usr/bin/chromium"})
	if err != nil {
		t.Fatal(err)
	}
	profile := session.Profile{ID: "missing", Backend: "browser", Environment: "service", URL: server.URL}
	if err := adapter.SelectProfile(profile); err != nil {
		t.Fatal(err)
	}
	lease, err := adapter.Acquire(context.Background(), 1280, 720, 30, 300)
	if err != nil {
		t.Fatal(err)
	}
	leaseID := lease["lease_id"].(string)
	if _, err := adapter.Launch(context.Background(), leaseID, profile); err == nil {
		t.Fatal("HTTP 404 launch succeeded")
	}
	if _, err := adapter.Release(context.Background(), leaseID); err != nil {
		t.Fatal(err)
	}

	badWorker := filepath.Join(t.TempDir(), "bad-worker.mjs")
	if err := os.WriteFile(badWorker, []byte("process.stdout.write('not-json\\n')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failing, err := New(Config{State: t.TempDir(), Worker: badWorker, Chromium: "/usr/bin/chromium"})
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.SelectProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Acquire(context.Background(), 1280, 720, 30, 300); err == nil {
		t.Fatal("malformed worker acquired a browser lease")
	}
	if err := failing.SelectProfile(profile); err != nil {
		t.Fatalf("failed acquire retained a private lease: %v", err)
	}
}
