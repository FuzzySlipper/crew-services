package session

import (
	"context"
	"encoding/json"
	"time"
)

// presentationObservation records a separate HTTP observation, never the
// identity of the native screenshot. Failures leave generic capture usable.
func (s *Service) presentationObservation(ctx context.Context, id string, enabled *bool) map[string]any {
	s.mu.Lock()
	st, err := s.require(id)
	var p Profile
	if err == nil {
		p, err = s.profile(st.Game)
	}
	s.mu.Unlock()
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	selected := p.PresentationObservations
	if enabled != nil {
		selected = *enabled
	}
	if !selected {
		return map[string]any{"available": false, "reason": "not requested; set engine_presentation or profile presentation_observations"}
	}
	started := time.Now().UTC()
	result, err := s.debugQuery(ctx, id, p.URL, "engine.renderer.presentation", "presentation", "Engine browser feedback; separate from screenshot")
	receipt, _ := result.(map[string]any)
	if receipt == nil {
		receipt = map[string]any{"command": "engine.renderer.presentation"}
	}
	receipt["observation_requested_at"] = started
	receipt["observation_completed_at"] = time.Now().UTC()
	if err != nil {
		receipt["error"] = err.Error()
	}
	if facts, ok := receipt["facts"].(map[string]any); ok {
		receipt["available"] = facts["available"] == true
	} else {
		receipt["available"] = false
	}
	return receipt
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func jsonSame(a, b any) any {
	if a == nil || b == nil {
		return "unknown"
	}
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	if ex != nil || ey != nil {
		return "unknown"
	}
	return string(x) == string(y)
}

// usablePresentation fences comparisons against stale runtime feedback. Pending
// observations may still contain useful last-submitted facts; retain their state.
func usablePresentation(receipt any) (map[string]any, map[string]any, bool) {
	r := object(receipt)
	f := object(r["facts"])
	p := object(f["presentation"])
	current := object(f["runtime"])
	observed := object(f["observationRuntime"])
	bound := true
	for _, key := range []string{"instanceId", "generation", "controlRevision"} {
		if jsonSame(current[key], observed[key]) != true {
			bound = false
		}
	}
	valid := r["error"] == nil && f["available"] == true && bound && p["surfaceId"] != nil && p["state"] != "unavailable" && object(p["submitted"]) != nil
	return f, p, valid
}

func presentationComparison(before, after any) map[string]any {
	a, ap, av := usablePresentation(before)
	b, bp, bv := usablePresentation(after)
	out := map[string]any{"same_runtime": "unknown", "same_surface": "unknown", "same_submitted_cameras": "unknown", "same_submitted_view_layout": "unknown", "same_submitted_viewport": "unknown", "same_submitted_view_revision": "unknown", "frame_correlation": "unavailable"}
	out["baseline_available"] = av
	out["current_available"] = bv
	if !av || !bv {
		return out
	}
	out["baseline_state"] = ap["state"]
	out["current_state"] = bp["state"]
	out["same_runtime"] = jsonSame(a["runtime"], b["runtime"])
	out["same_surface"] = jsonSame(ap["surfaceId"], bp["surfaceId"])
	as, bs := object(ap["submitted"]), object(bp["submitted"])
	aw, bw := object(as["views"]), object(bs["views"])
	// Compare composition data, excluding its continuously advancing local revision.
	out["same_submitted_cameras"] = jsonSame(aw["cameras"], bw["cameras"])
	out["same_submitted_view_layout"] = jsonSame(aw["views"], bw["views"])
	out["same_submitted_viewport"] = jsonSame(as["viewport"], bs["viewport"])
	out["same_submitted_fallback_camera"] = jsonSame(as["fallbackCamera"], bs["fallbackCamera"])
	if out["same_runtime"] == true && out["same_surface"] == true {
		out["same_submitted_view_revision"] = jsonSame(as["viewRevision"], bs["viewRevision"])
	}
	out["meaning"] = "separate submitted observations; camera equality does not correlate either PNG"
	return out
}
