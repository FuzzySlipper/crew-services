package session

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestCapturePreservesOriginalAndDoesNotInventComparability(t *testing.T) {
	s, _, id := testService(t)
	first, err := s.Capture(context.Background(), id, json.RawMessage(`{"viewpoint":{"name":"entrance"}}`))
	if err != nil {
		t.Fatal(err)
	}
	a := first.(map[string]any)
	if a["path"] != "frame.png" || a["frame_correlation"] != "unavailable" {
		t.Fatalf("invented evidence: %v", a)
	}
	data, _ := json.Marshal(map[string]any{"compare_to": a["capture_id"], "viewpoint": map[string]any{"name": "entrance"}})
	next, err := s.Capture(context.Background(), id, data)
	if err != nil {
		t.Fatal(err)
	}
	pair := next.(map[string]any)["comparison"].(map[string]any)
	if pair["same_pixel_dimensions"] != "unknown" || pair["same_supplied_viewpoint"] != true {
		t.Fatalf("unknown geometry must not match: %v", pair)
	}
	if _, err := os.Stat(a["metadata_path"].(string)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Capture(context.Background(), id, json.RawMessage(`{"overlay_policy":"hide"}`)); err == nil {
		t.Fatal("silently hid overlays")
	}
	if _, err = s.Capture(context.Background(), id, json.RawMessage(`{"compare_to":"../../outside"}`)); err == nil {
		t.Fatal("accepted non-artifact path")
	}
}
