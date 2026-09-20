package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJevUsesTypedDecisionAndPreservesProbability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" {
			t.Error(r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "jev" || body["messages"] != nil {
			t.Error(body)
		}
		q := body["questions"].(map[string]any)["next_action"].(map[string]any)
		if q["criteria"].(map[string]any)["forward"] != "move" {
			t.Error(q)
		}
		w.Write([]byte(`{"answers":{"next_action":{"type":"choice","choice":"forward","confidence":0.82,"probabilities":{"forward":0.9}}}}`))
	}))
	defer server.Close()
	j := Jev{BaseURL: server.URL, Model: "jev"}
	d, e := j.Decide(context.Background(), State{}, []Tactic{{ID: "forward", Description: "move"}})
	if e != nil || d.Choice != "forward" || d.Confidence != .82 || len(d.Raw) == 0 {
		t.Fatalf("%+v %v", d, e)
	}
}
