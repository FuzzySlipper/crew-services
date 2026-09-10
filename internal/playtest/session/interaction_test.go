package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestInteractionProductContract(t *testing.T) {
	var commands []string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__rusty/product/runtime/debug/catalog":
			io.WriteString(w, `{"available":true,"commands":[{"name":"interaction.query","parameters":[]},{"name":"interaction.cursor","parameters":[{"name":"x"},{"name":"y"},{"name":"aspect"}]}]}`)
		case "/__rusty/product/runtime/debug/execute":
			raw, _ := io.ReadAll(r.Body)
			commands = append(commands, string(raw))
			io.WriteString(w, `{"mode":"reticle","semanticTargeting":true,"lookAssistance":false,"selected":18446744073709551614,"candidates":[{"reason":"OutOfReach","route":"Unknown","visibility":"Occluded"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	s, err := New(&fakeBackend{}, nil, []Profile{{ID: "fixture", URL: fixture.URL + "/nested/game", InteractionQueries: true}}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s.current = "lease"
	s.sessions["lease"] = &Session{ID: "lease", Game: "fixture", Phase: "connected"}
	for _, options := range []string{`{}`, `{"mode":"cursor","x":0.2,"y":0.4,"aspect":1.5}`} {
		result, err := s.Interaction(context.Background(), "lease", json.RawMessage(options))
		if err != nil {
			t.Fatal(err)
		}
		receipt := result.(map[string]any)
		facts := receipt["facts"].(map[string]any)
		if facts["selected"].(json.Number).String() != "18446744073709551614" {
			t.Fatal("target identity lost precision")
		}
		data, err := os.ReadFile(receipt["evidence_path"].(string))
		if err != nil || !strings.Contains(string(data), "OutOfReach") {
			t.Fatalf("missing durable product facts: %s %v", data, err)
		}
	}
	if len(commands) != 2 || commands[0] != "interaction.query" || commands[1] != "interaction.cursor 0.2 0.4 1.5" {
		t.Fatal(commands)
	}
	for _, options := range []string{`{"mode":"use"}`, `{"mode":"cursor","x":0.5,"y":0.5}`, `{"command":"loot"}`, `{} {}`, `{"x":0.5}`} {
		if _, err := s.Interaction(context.Background(), "lease", json.RawMessage(options)); err == nil {
			t.Fatalf("accepted %s", options)
		}
	}
	if len(commands) != 2 {
		t.Fatal("invalid options reached product")
	}
	s.profiles[0].InteractionQueries = false
	if _, err := s.Interaction(context.Background(), "lease", nil); err == nil || !strings.Contains(err.Error(), "capability_unavailable") {
		t.Fatal(err)
	}
}

func TestInteractionMissingCatalogAndCancelled(t *testing.T) {
	posts := 0
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		io.WriteString(w, `{"available":true,"commands":[]}`)
	}))
	defer fixture.Close()
	s, _ := New(&fakeBackend{}, nil, []Profile{{ID: "fixture", URL: fixture.URL, InteractionQueries: true}}, t.TempDir(), "")
	s.current = "lease"
	s.sessions["lease"] = &Session{ID: "lease", Game: "fixture", Phase: "connected"}
	if _, err := s.Interaction(context.Background(), "lease", nil); err == nil || !strings.Contains(err.Error(), "capability_unavailable") {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("executed unadvertised command")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Interaction(ctx, "lease", nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}
