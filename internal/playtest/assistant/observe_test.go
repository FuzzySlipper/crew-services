package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crew-services/internal/playtest/client"
)

func TestObserverCapturesAndReturnsRawProductFacts(t *testing.T) {
	var executed []string
	product := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__rusty/product/runtime/debug/catalog":
			if r.Method != http.MethodGet {
				t.Fatalf("catalog method = %s", r.Method)
			}
			_, _ = io.WriteString(w, `{"available":true,"commands":[{"name":"spatial.map"},{"name":"loading-bay.readout"}]}`)
		case "/__rusty/product/runtime/debug/execute":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("execute request = %s %q", r.Method, r.Header.Get("Content-Type"))
			}
			body, _ := io.ReadAll(r.Body)
			executed = append(executed, string(body))
			if string(body) == "spatial.map json 12 0.5" {
				_, _ = io.WriteString(w, `{"entity":18446744073709551614,"cells":[1,2]}`)
				return
			}
			_, _ = io.WriteString(w, "generation=2;step=4")
		default:
			t.Fatalf("unexpected product request %s", r.URL.Path)
		}
	}))
	defer product.Close()
	playtest := newObserveService(t, `{"path":"/tmp/original.png","width":1280}`)
	defer playtest.Close()

	service, err := client.New(playtest.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := (Observer{Client: service, SessionID: "session-1", ProductURL: product.URL + "/game", Commands: []string{" spatial.map   json 12 0.5 ", "loading-bay.readout"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(observation.Capture) != `{"path":"/tmp/original.png","width":1280}` {
		t.Fatalf("capture = %s", observation.Capture)
	}
	if got := string(observation.Facts["spatial.map json 12 0.5"]); got != `{"entity":18446744073709551614,"cells":[1,2]}` {
		t.Fatalf("spatial fact = %s", got)
	}
	if got := string(observation.Facts["loading-bay.readout"]); got != `"generation=2;step=4"` {
		t.Fatalf("readout fact = %s", got)
	}
	if strings.Join(executed, ",") != "spatial.map json 12 0.5,loading-bay.readout" {
		t.Fatalf("executed = %v", executed)
	}
}

func TestObserverRejectsCommandsOutsideReadOnlyAllowlist(t *testing.T) {
	playtest := newObserveService(t, `{"path":"/tmp/original.png"}`)
	defer playtest.Close()
	service, err := client.New(playtest.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"loading-bay.set-track health 1", "spatial.map ascii 12 1", "spatial.map json 16 1", "spatial.map json 2 0", "spatial.map json NaN 1"} {
		_, err := (Observer{Client: service, SessionID: "session-1", Commands: []string{command}}).Observe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "command") && !strings.Contains(err.Error(), "spatial.map") {
			t.Fatalf("command %q error = %v", command, err)
		}
	}
}

func TestObserverRequiresCatalogCommandAndDoesNotExecuteIt(t *testing.T) {
	posts := 0
	product := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		_, _ = io.WriteString(w, `{"available":true,"commands":[{"name":"loading-bay.readout"}]}`)
	}))
	defer product.Close()
	playtest := newObserveService(t, `{"path":"/tmp/original.png"}`)
	defer playtest.Close()
	service, err := client.New(playtest.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (Observer{Client: service, SessionID: "session-1", ProductURL: product.URL, Commands: []string{"spatial.map json 2 1"}}).Observe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "capability_unavailable") {
		t.Fatalf("error = %v", err)
	}
	if posts != 0 {
		t.Fatal("executed a command absent from catalog")
	}
}

func newObserveService(t *testing.T, result string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			t.Fatalf("playtest request = %s %s", r.Method, r.URL.Path)
		}
		var request client.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Op != "observe" || request.SessionID != "session-1" {
			t.Fatalf("playtest command = %+v", request)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":`+result+`}`)
	}))
}
