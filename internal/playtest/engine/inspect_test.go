package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectionRequestsBecomeEngineCommands(t *testing.T) {
	handle := uint64(12)
	for _, c := range []struct {
		request Request
		command string
	}{
		{Request{Op: "view"}, "engine.inspect.view list"},
		{Request{Op: "view", Mode: "targets"}, "engine.inspect.target list"},
		{Request{Op: "view", Mode: "create", ID: "side"}, "engine.inspect.view create side"},
		{Request{Op: "view", Mode: "create", ID: "side", Anchor: "held.pane"}, "engine.inspect.view create side anchor held.pane"},
		{Request{Op: "view", Mode: "close", ID: "side"}, "engine.inspect.view close side"},
		{Request{Op: "view", Mode: "orbit", ID: "side", Yaw: ptr(30.0), Pitch: ptr(-10.0)}, "engine.inspect.view orbit side 30 -10"},
		{Request{Op: "view", Mode: "pan", ID: "side", X: ptr(0.1), Y: ptr(0.0)}, "engine.inspect.view pan side 0.1 0"},
		{Request{Op: "view", Mode: "zoom", ID: "side", Factor: ptr(0.5)}, "engine.inspect.view zoom side 0.5"},
		{Request{Op: "view", Mode: "camera", ID: "side", Camera: json.RawMessage(`{"position":[1,2,3],"yawDegrees":90,"pitchDegrees":-5,"fovDegrees":40}`)}, "engine.inspect.view camera side 1 2 3 90 -5 40"},
		{Request{Op: "view", Mode: "frame", ID: "side", Framing: "side"}, "engine.inspect.view frame side side camera"},
		{Request{Op: "view", Mode: "frame", ID: "side", Framing: "front", Handle: &handle}, "engine.inspect.view frame side front 12"},
		{Request{Op: "view", Mode: "frame", ID: "side", Framing: "underside", Target: "off-grip"}, "engine.inspect.view frame side underside target off-grip"},
		{Request{Op: "overlay"}, "engine.inspect.overlay list"},
		{Request{Op: "overlay", Mode: "show"}, "engine.inspect.overlay show"},
		{Request{Op: "overlay", Mode: "hide", Kind: "sets"}, "engine.inspect.overlay hide sets"},
		{Request{Op: "overlay", Mode: "show", Kind: "sets", View: "side"}, "engine.inspect.overlay show sets in side"},
		{Request{Op: "overlay", Mode: "show", Kind: "set", Name: "inspect.targets", View: "player"}, "engine.inspect.overlay show set inspect.targets in player"},
		{Request{Op: "overlay", Mode: "show", Kind: "joints", Handle: &handle}, "engine.inspect.overlay show joints 12"},
		{Request{Op: "overlay", Mode: "show", Kind: "debug-layer"}, "engine.inspect.overlay show debug-layer"},
		{Request{Op: "overlay", Mode: "show", Kind: "collision", X: ptr(1.0), Y: ptr(0.0), Z: ptr(-2.0), Distance: ptr(6.0)}, "engine.inspect.overlay show collision at 1 0 -2 6"},
		{Request{Op: "scene"}, "engine.scene.objects"},
		{Request{Op: "scene", Depth: ptr(0)}, "engine.scene.objects * 0"},
		{Request{Op: "scene", Handle: &handle, Depth: ptr(2), Layer: "viewmodel", Page: ptr(1)}, "engine.scene.objects 12 2 --layer viewmodel --page 1"},
		{Request{Op: "scene", Mode: "object", Handle: &handle}, "engine.scene.object 12"},
		{Request{Op: "scene", Mode: "composition"}, "engine.scene.composition"},
		{Request{Op: "scene", Mode: "resources"}, "engine.scene.resources"},
	} {
		got, err := inspectCommand(c.request)
		if err != nil || got != c.command {
			t.Errorf("%+v: %q %v, want %q", c.request, got, err, c.command)
		}
	}
	for _, bad := range []Request{
		{Op: "view", Mode: "create"},
		{Op: "view", Mode: "create", ID: "side", Anchor: "two words"},
		{Op: "view", Mode: "frame", ID: "side", Framing: "sideways"},
		{Op: "view", Mode: "frame", ID: "side", Framing: "side", Target: "a;b"},
		{Op: "view", Mode: "camera", ID: "side", Camera: json.RawMessage(`{"position":[1,2,3]}`)},
		{Op: "view", Mode: "zoom", ID: "side"},
		{Op: "overlay", Mode: "show", Kind: "set"},
		{Op: "overlay", Mode: "show", Kind: "bounds"},
		{Op: "overlay", Mode: "show", Kind: "camera", View: "side"},
		{Op: "overlay", Mode: "toggle"},
		{Op: "scene", Mode: "object"},
		{Op: "scene", Layer: "hud"},
	} {
		if got, err := inspectCommand(bad); err == nil {
			t.Errorf("%+v accepted as %q", bad, got)
		}
	}
}

// An agent opens a side view of the held assembly, shows overlays, captures
// the player view and the inspection view at one held step, and closes it.
func TestViewCapturesThePlayerAndTheViewAtOneHeldStep(t *testing.T) {
	fake, host := startFake(t)
	directory := t.TempDir()
	r := &runner{host: host, directory: directory}
	run := func(q Request) map[string]any {
		t.Helper()
		result, err := r.run(context.Background(), q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return result
	}
	run(Request{Op: "time", Mode: "manual"})
	run(Request{Op: "advance", MS: ptr(100.0)})
	run(Request{Op: "view", Mode: "create", ID: "side"})
	run(Request{Op: "view", Mode: "frame", ID: "side", Framing: "side"})
	run(Request{Op: "overlay", Mode: "show", Kind: "sets", View: "side"})
	result := run(Request{Op: "view", Mode: "capture", ID: "side", Player: true, Overlays: true, Width: 4, Height: 2})
	if result["sameStep"] != true {
		t.Fatalf("captures at different steps: %v", result)
	}
	view, player := result["view"].(map[string]any), result["player"].(map[string]any)
	if view["step"] != uint64(fake.step) || !strings.HasPrefix(filepath.Base(view["path"].(string)), "view-side-step-6-") ||
		!strings.HasPrefix(filepath.Base(player["path"].(string)), "player-step-6-") {
		t.Fatalf("capture names: %v %v", view["path"], player["path"])
	}
	headers := view["headers"].(map[string]string)
	if headers["X-Rusty-Frame-View"] == "" || headers["X-Rusty-Frame-Overlays"] != `{"inspect.targets":3}` {
		t.Fatalf("view headers: %v", headers)
	}
	if facts := view["frame"].(map[string]any); facts["view"].(map[string]any)["id"] != "side" || facts["held"] != true {
		t.Fatalf("view frame facts: %v", facts)
	}
	if _, ok := player["headers"].(map[string]string)["X-Rusty-Frame-View"]; ok {
		t.Fatal("the player capture carried a view header")
	}
	// Evidence: each PNG has a sidecar with the headers.
	sidecar, err := os.ReadFile(view["metadataPath"].(string))
	if err != nil || !strings.Contains(string(sidecar), "X-Rusty-Frame-Overlays") {
		t.Fatalf("sidecar: %s %v", sidecar, err)
	}
	if fake.captures[0] != "format=png&height=2&overlays=1&view=side&width=4" || fake.captures[1] != "format=png&height=2&overlays=1&width=4" {
		t.Fatalf("capture queries: %v", fake.captures)
	}
	run(Request{Op: "view", Mode: "close", ID: "side"})
	want := []string{"engine.inspect.view create side", "engine.inspect.view frame side side camera", "engine.inspect.overlay show sets in side", "engine.inspect.view close side"}
	var got []string
	for _, command := range fake.commands {
		if strings.HasPrefix(command, "engine.inspect.") {
			got = append(got, command)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands %v", got)
	}
}

func TestEngineRefusalsPassThroughAsWorded(t *testing.T) {
	_, host := startFake(t)
	r := &runner{host: host, directory: t.TempDir()}
	_, err := r.run(context.Background(), Request{Op: "view", Mode: "close", ID: "gone"})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Detail != "INSPECT_NO_VIEW: no inspection view gone" {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := r.run(context.Background(), Request{Op: "view", Mode: "capture", ID: "gone"}); err == nil || !strings.Contains(err.Error(), "INSPECT_NO_VIEW: no inspection view gone") {
		t.Fatalf("capture refusal: %v", err)
	}
}
