package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryReloadKeepsActiveQueryOriginAndControls(t *testing.T) {
	calls := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "catalog") {
			w.Write([]byte(`{"available":true,"commands":[{"name":"interaction.query"},{"name":"engine.renderer.presentation"}]}`))
			return
		}
		w.Write([]byte(`{"available":true}`))
	}))
	defer host.Close()
	profile := Profile{ID: "a", URL: host.URL, InteractionQueries: true, PresentationObservations: true, Controls: map[string]string{"W": "move"}}
	r := NewRegistry([]Profile{profile})
	s, err := NewWithRegistry(&fakeBackend{}, fakeLauncher{}, r, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Start(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	st := v.(*Session)
	profile.Controls["W"] = "caller mutation"
	snapshot := r.Profiles()
	snapshot[0].Controls["W"] = "reader mutation"
	// Remove A entirely. Both query routes must still use its original opt-ins
	// and URL, and the durable session must retain its controls.
	if err := r.Replace([]Profile{{ID: "b", URL: "http://127.0.0.1:1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Interaction(context.Background(), st.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(context.Background(), st.ID, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || st.Profile.Controls["W"] != "move" {
		t.Fatalf("calls=%d profile=%+v", calls, st.Profile)
	}
	if _, err := s.ManualInput(context.Background(), st.ID, []map[string]any{{"kind": "wait", "ms": 1}}); err != nil {
		t.Fatal(err)
	}
	// Restore an interrupted record with A absent from the new registry.
	restored, err := NewWithRegistry(&fakeBackend{}, fakeLauncher{}, r, s.stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := restored.SlotStatus()
	if got == nil || got.Profile.URL != host.URL {
		t.Fatal("lost durable profile snapshot")
	}
	s.Close(context.Background())
}
