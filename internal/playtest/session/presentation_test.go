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

func presentationFixture(surface, state, control string) map[string]any {
	return map[string]any{"available": true, "runtime": map[string]any{"instanceId": "1", "generation": "1", "controlRevision": "1"}, "observationRuntime": map[string]any{"instanceId": "1", "generation": "1", "controlRevision": control}, "observationAgeMs": 245, "presentation": map[string]any{"surfaceId": surface, "state": state, "pendingRealizations": 1, "submitted": map[string]any{"viewRevision": 9007199254740993, "publicationFrontiers": []any{map[string]any{"stream": "world", "revision": 9007199254740993}}, "viewport": map[string]any{"cssWidth": 1280, "backingWidth": 640}, "views": map[string]any{"cameras": []any{map[string]any{"id": "primary", "pose": map[string]any{"position": []int{0, 1, 2}}}}, "views": []any{map[string]any{"cameraId": "primary"}}}}}}
}

func TestPresentationCaptureKeepsPendingFactsAndOriginal(t *testing.T) {
	current := presentationFixture("surface-1", "pending", "1")
	queryCount := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "catalog") {
			io.WriteString(w, `{"available":true,"commands":[{"name":"engine.renderer.presentation"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != "engine.renderer.presentation" {
			t.Errorf("unexpected command %s", raw)
		}
		queryCount++
		json.NewEncoder(w).Encode(current)
	}))
	defer host.Close()
	s, _, id := testService(t)
	s.profiles[0].URL = host.URL
	s.profiles[0].PresentationObservations = true
	first, err := s.Capture(context.Background(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := first.(map[string]any)
	if a["path"] != "frame.png" || a["frame_correlation"] != "unavailable" || a["engine_readiness"] != "unavailable" || a["engine_presentation_state"] != "pending" {
		t.Fatal(a)
	}
	receipt := object(a["engine_presentation"])
	if receipt["observation_requested_at"] == nil || receipt["observation_completed_at"] == nil {
		t.Fatal(receipt)
	}
	raw, _ := os.ReadFile(a["metadata_path"].(string))
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("lost revision precision")
	}
	options, _ := json.Marshal(map[string]any{"compare_to": a["capture_id"]})
	second, err := s.Capture(context.Background(), id, options)
	if err != nil {
		t.Fatal(err)
	}
	comparison := object(object(second.(map[string]any)["comparison"])["engine_presentation"])
	if comparison["same_submitted_view_revision"] != true || comparison["same_submitted_cameras"] != true {
		t.Fatal(comparison)
	}
	current = presentationFixture("surface-2", "submitted", "1")
	third, err := s.Capture(context.Background(), id, options)
	if err != nil {
		t.Fatal(err)
	}
	comparison = object(object(third.(map[string]any)["comparison"])["engine_presentation"])
	if comparison["same_surface"] != false || comparison["same_submitted_view_revision"] != "unknown" {
		t.Fatal("compared revisions across surfaces", comparison)
	}
	current = presentationFixture("surface-2", "submitted", "0")
	stale, err := s.Capture(context.Background(), id, options)
	if err != nil {
		t.Fatal(err)
	}
	if stale.(map[string]any)["engine_presentation_state"] != "unavailable" {
		t.Fatal("accepted stale binding")
	}
	_, err = s.Capture(context.Background(), id, json.RawMessage(`{"engine_presentation":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if queryCount != 4 {
		t.Fatal("override did not disable querying", queryCount)
	}
}

func TestPresentationUnavailableDoesNotLoseImage(t *testing.T) {
	for _, response := range []string{`{"available":false,"presentation":null}`, `not json`} {
		t.Run(response, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"available":true,"commands":[{"name":"engine.renderer.presentation"}]}`)
				} else {
					io.WriteString(w, response)
				}
			}))
			defer host.Close()
			s, _, id := testService(t)
			s.profiles[0].URL = host.URL
			result, err := s.Capture(context.Background(), id, json.RawMessage(`{"engine_presentation":true}`))
			if err != nil {
				t.Fatal(err)
			}
			capture := result.(map[string]any)
			if capture["path"] != "frame.png" || capture["engine_presentation_state"] != "unavailable" {
				t.Fatal(capture)
			}
			receipt := object(capture["engine_presentation"])
			if receipt["raw_result"] != response {
				t.Fatal("lost raw observation", receipt)
			}
		})
	}
}
