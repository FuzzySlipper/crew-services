package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParentParsesResponsesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		text := `{"situation":"enemy ahead","objective":"hold range","target":"1","parameters":{"distance":6},"preferred_tactics":["forward"],"stop":false}`
		event, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
		fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n", event)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Model: "parent", Protocol: "responses"}
	d, e := p.Advise(context.Background(), State{}, []Tactic{{ID: "forward"}})
	if e != nil || d.Guidance.Objective != "hold range" {
		t.Fatalf("%+v %v", d, e)
	}
}
func TestIncompleteParentStreamCannotInstallGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"{}"}`)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Protocol: "responses"}
	if _, e := p.Advise(context.Background(), State{}, nil); e == nil {
		t.Fatal("accepted incomplete response")
	}
}
