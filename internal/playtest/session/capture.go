package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

type CaptureOptions struct {
	EnginePresentation *bool    `json:"engine_presentation,omitempty"`
	Label              string   `json:"label,omitempty"`
	CompareTo          string   `json:"compare_to,omitempty"`
	Viewpoint          any      `json:"viewpoint,omitempty"`
	Assistance         []string `json:"assistance,omitempty"`
	OverlayPolicy      string   `json:"overlay_policy,omitempty"`
}

// Capture publishes a sidecar referencing the original observation. Supplied
// pose/assistance descriptions remain assertions by the caller, not host facts.
func (s *Service) Capture(ctx context.Context, id string, data json.RawMessage) (any, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var o CaptureOptions
	if len(data) > 0 {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&o); err != nil {
			return nil, err
		}
	}
	if len(o.Label) > 256 || len(o.Assistance) > 32 {
		return nil, errors.New("capture label/assistance exceeds bounds")
	}
	if o.OverlayPolicy == "" {
		o.OverlayPolicy = "preserve"
	}
	if o.OverlayPolicy != "preserve" {
		return nil, errors.New("capability_unavailable: only preserve overlays is supported; product UI is never hidden")
	}
	var baseline map[string]any
	if o.CompareTo != "" {
		if _, err := uuid.Parse(o.CompareTo); err != nil {
			return nil, errors.New("compare_to must be a capture ID")
		}
		raw, err := os.ReadFile(filepath.Join(s.stateDir, "capture-"+o.CompareTo+".json"))
		if err != nil {
			return nil, fmt.Errorf("read baseline capture: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&baseline); err != nil {
			return nil, err
		}
	}
	var poolBefore any
	if s.poolActivity != nil {
		poolBefore = s.poolActivity()
	}
	engine := s.presentationObservation(ctx, id, o.EnginePresentation)
	observation, err := s.Observe(ctx, id)
	if err != nil {
		return nil, err
	}
	obs, ok := observation.(map[string]any)
	if !ok {
		return nil, errors.New("backend observation has no metadata object")
	}
	captureID := uuid.NewString()
	r := map[string]any{"capture_id": captureID, "session_id": id, "recorded_at": time.Now().UTC(), "label": o.Label, "path": obs["path"], "observation": obs, "supplied_viewpoint": o.Viewpoint, "supplied_assistance": o.Assistance, "supplied_metadata_source": "caller; not independently verified", "overlay_policy": o.OverlayPolicy, "engine_readiness": "unavailable", "frame_correlation": "unavailable"}
	if s.poolActivity != nil {
		r["pool_activity"] = map[string]any{"slot_id": s.slotID, "before": poolBefore, "after": s.poolActivity(), "performance_isolated": false}
	}
	r["engine_presentation"] = engine
	r["engine_presentation_state"] = "unavailable"
	if _, p, ok := usablePresentation(engine); ok {
		r["engine_presentation_state"] = p["state"]
	}
	r["gpu_completion"] = "unavailable"
	r["application_renderer"] = "unavailable"
	r["hardware_acceleration"] = "unavailable"
	if baseline != nil {
		before, _ := baseline["observation"].(map[string]any)
		// Normalize JSON numbers before comparing in-memory values with disk values.
		comparable := func(a, b any) any {
			if a == nil || b == nil {
				return "unknown"
			}
			x, _ := json.Marshal(a)
			y, _ := json.Marshal(b)
			return bytes.Equal(x, y)
		}
		var dimensions any = "unknown"
		if before["width"] != nil && before["height"] != nil && obs["width"] != nil && obs["height"] != nil {
			dimensions = comparable(before["width"], obs["width"]) == true && comparable(before["height"], obs["height"]) == true
		}
		r["comparison"] = map[string]any{"baseline_capture_id": o.CompareTo, "baseline_path": baseline["path"], "same_pixel_dimensions": dimensions, "same_supplied_viewpoint": comparable(baseline["supplied_viewpoint"], o.Viewpoint), "same_observed_viewport": comparable(before["viewport"], obs["viewport"]), "same_observed_canvases": comparable(before["canvases"], obs["canvases"]), "visual_verdict": "not assessed; inspect both original images", "readiness_comparable": "unknown"}
		r["comparison"].(map[string]any)["engine_presentation"] = presentationComparison(baseline["engine_presentation"], engine)
	}
	path := filepath.Join(s.stateDir, "capture-"+captureID+".json")
	r["metadata_path"] = path
	if err := atomicJSON(path, r); err != nil {
		return r, fmt.Errorf("capture metadata persistence failed; original image retained: %w", err)
	}
	return r, nil
}
