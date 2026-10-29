package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"crew-services/internal/playtest/evidence"
	"github.com/google/uuid"
)

// Inspection views, overlays and scene readouts wrap the Engine's
// engine.inspect.* and engine.scene.* live-debug commands. crew-services
// checks only that each argument is one well-formed word; the Engine decides
// what it means, and its refusals pass through as it words them.

// name is an overlay set or inspection target: one word, dots allowed.
var name = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var framings = map[string]bool{"front": true, "back": true, "left": true, "right": true, "side": true, "top": true, "underside": true, "close": true}

var sceneLayers = map[string]bool{"scene": true, "debug": true, "ui": true, "viewmodel": true, "backdrop": true}

type viewCamera struct {
	Position     *[3]float64 `json:"position"`
	YawDegrees   *float64    `json:"yawDegrees"`
	PitchDegrees *float64    `json:"pitchDegrees"`
	FovDegrees   *float64    `json:"fovDegrees,omitempty"`
}

// inspectCommand turns a view (other than capture), overlay or scene request
// into its debug command.
func inspectCommand(q Request) (string, error) {
	switch q.Op {
	case "view":
		return viewCommand(q)
	case "overlay":
		return overlayCommand(q)
	case "scene":
		return sceneCommand(q)
	}
	return "", fmt.Errorf("unknown inspection operation %q", q.Op)
}

func viewCommand(q Request) (string, error) {
	const command = "engine.inspect.view"
	needID := func() error {
		if q.ID == "" {
			return fmt.Errorf(`view %s requires id; example: {op:"view",mode:"create",id:"side"}`, q.Mode)
		}
		return nil
	}
	switch q.Mode {
	case "", "list":
		return command + " list", nil
	case "targets":
		return "engine.inspect.target list", nil
	case "create":
		if err := needID(); err != nil {
			return "", err
		}
		if q.Anchor == "" {
			return command + " create " + q.ID, nil
		}
		if !name.MatchString(q.Anchor) {
			return "", errors.New("view anchor must be one word")
		}
		return command + " create " + q.ID + " anchor " + q.Anchor, nil
	case "close", "select":
		if err := needID(); err != nil {
			return "", err
		}
		return command + " " + q.Mode + " " + q.ID, nil
	case "camera":
		if err := needID(); err != nil {
			return "", err
		}
		var camera viewCamera
		if len(q.Camera) == 0 || json.Unmarshal(q.Camera, &camera) != nil || camera.Position == nil || camera.YawDegrees == nil || camera.PitchDegrees == nil {
			return "", errors.New(`view camera requires camera {position:[x,y,z],yawDegrees,pitchDegrees[,fovDegrees]}`)
		}
		p := camera.Position
		values := []float64{p[0], p[1], p[2], *camera.YawDegrees, *camera.PitchDegrees}
		if camera.FovDegrees != nil {
			values = append(values, *camera.FovDegrees)
		}
		if !finite(values...) {
			return "", errors.New("view camera must be finite")
		}
		return command + " camera " + q.ID + " " + words(values...), nil
	case "orbit":
		if err := needID(); err != nil {
			return "", err
		}
		yaw, pitch := floatOr(q.Yaw, 0), floatOr(q.Pitch, 0)
		if !finite(yaw, pitch) {
			return "", errors.New("view orbit yaw/pitch must be finite degrees")
		}
		return command + " orbit " + q.ID + " " + words(yaw, pitch), nil
	case "pan":
		if err := needID(); err != nil {
			return "", err
		}
		dx, dy := floatOr(q.X, 0), floatOr(q.Y, 0)
		if !finite(dx, dy) {
			return "", errors.New("view pan x/y must be finite")
		}
		return command + " pan " + q.ID + " " + words(dx, dy), nil
	case "zoom":
		if err := needID(); err != nil {
			return "", err
		}
		if q.Factor == nil || !finite(*q.Factor) {
			return "", errors.New("view zoom requires a finite factor")
		}
		return command + " zoom " + q.ID + " " + words(*q.Factor), nil
	case "frame":
		if err := needID(); err != nil {
			return "", err
		}
		if !framings[q.Framing] {
			return "", errors.New("view frame requires framing front|back|left|right|side|top|underside|close")
		}
		subject, err := frameSubject(q)
		if err != nil {
			return "", err
		}
		return command + " frame " + q.ID + " " + q.Framing + " " + subject, nil
	}
	return "", fmt.Errorf("unknown view mode %q; list|targets|create|close|select|camera|orbit|pan|zoom|frame|capture", q.Mode)
}

// frameSubject is a scene handle, a product inspection target, or (by
// default) the space just in front of the player's camera.
func frameSubject(q Request) (string, error) {
	switch {
	case q.Handle != nil && q.Target != "":
		return "", errors.New("frame names a handle or a target, not both")
	case q.Handle != nil:
		return fmt.Sprint(*q.Handle), nil
	case q.Target != "":
		if !name.MatchString(q.Target) {
			return "", errors.New("inspection target must be one word")
		}
		return "target " + q.Target, nil
	}
	return "camera", nil
}

func overlayCommand(q Request) (string, error) {
	const command = "engine.inspect.overlay"
	switch q.Mode {
	case "", "list":
		if q.Kind != "" {
			return "", errors.New("overlay list takes no kind")
		}
		return command + " list", nil
	case "show", "hide":
	default:
		return "", fmt.Errorf("unknown overlay mode %q; list|show|hide", q.Mode)
	}
	base := command + " " + q.Mode
	in := ""
	if q.View != "" {
		if q.Kind != "sets" && q.Kind != "set" {
			return "", errors.New("overlay view applies to kind sets or set")
		}
		if !identifier.MatchString(q.View) {
			return "", errors.New("overlay view must be an inspection view id or player")
		}
		in = " in " + q.View
	}
	switch q.Kind {
	case "":
		return base, nil
	case "sets":
		return base + " sets" + in, nil
	case "set":
		if !name.MatchString(q.Name) {
			return "", errors.New("overlay set requires name, one word")
		}
		return base + " set " + q.Name + in, nil
	case "debug-layer", "camera", "motion":
		return base + " " + q.Kind, nil
	case "joints", "bounds", "ik":
		if q.Handle == nil {
			return "", fmt.Errorf("overlay %s requires handle from scene objects", q.Kind)
		}
		return fmt.Sprintf("%s %s %d", base, q.Kind, *q.Handle), nil
	case "collision":
		parts := base + " collision"
		if q.X != nil || q.Y != nil || q.Z != nil {
			if q.X == nil || q.Y == nil || q.Z == nil || !finite(*q.X, *q.Y, *q.Z) {
				return "", errors.New("overlay collision at needs finite x/y/z")
			}
			parts += " at " + words(*q.X, *q.Y, *q.Z)
		}
		if q.Distance != nil {
			if !finite(*q.Distance) {
				return "", errors.New("overlay collision distance must be finite metres")
			}
			parts += " " + words(*q.Distance)
		}
		return parts, nil
	}
	return "", fmt.Errorf("unknown overlay kind %q; sets|set|debug-layer|camera|motion|joints|bounds|ik|collision", q.Kind)
}

func sceneCommand(q Request) (string, error) {
	switch q.Mode {
	case "", "objects":
		command := "engine.scene.objects"
		if q.Handle != nil {
			command += fmt.Sprintf(" %d", *q.Handle)
		} else if q.Depth != nil {
			command += " *"
		}
		if q.Depth != nil {
			if *q.Depth < 0 {
				return "", errors.New("scene depth must be 0 or more")
			}
			command += fmt.Sprintf(" %d", *q.Depth)
		}
		if q.Layer != "" {
			if !sceneLayers[q.Layer] {
				return "", errors.New("scene layer is scene|debug|ui|viewmodel|backdrop")
			}
			command += " --layer " + q.Layer
		}
		if q.Page != nil {
			if *q.Page < 0 {
				return "", errors.New("scene page must be 0 or more")
			}
			command += fmt.Sprintf(" --page %d", *q.Page)
		}
		return command, nil
	case "object":
		if q.Handle == nil {
			return "", errors.New(`scene object requires handle; example: {op:"scene",mode:"object",handle:12}`)
		}
		return fmt.Sprintf("engine.scene.object %d", *q.Handle), nil
	case "composition", "resources":
		return "engine.scene." + q.Mode, nil
	}
	return "", fmt.Errorf("unknown scene mode %q; objects|object|composition|resources", q.Mode)
}

func words(values ...float64) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return strings.Join(parts, " ")
}

// view runs an inspection view command, or captures the view. A capture with
// player also captures the product's composition, so both show the step
// they were drawn at; hold time first to make that one step.
func (r *runner) view(ctx context.Context, q Request) (map[string]any, error) {
	if q.Mode != "capture" {
		command, err := viewCommand(q)
		if err != nil {
			return nil, err
		}
		return r.host.Debug(ctx, command)
	}
	if q.ID == "" {
		return nil, errors.New(`view capture requires id; example: {op:"view",mode:"capture",id:"side",player:true,overlays:true}`)
	}
	shot, err := r.inspectionCapture(ctx, q.ID, q)
	if err != nil {
		return nil, err
	}
	if !q.Player {
		return shot, nil
	}
	player, err := r.inspectionCapture(ctx, "", q)
	if err != nil {
		shot["playerError"] = err.Error()
		return shot, nil
	}
	result := map[string]any{"view": shot, "player": player, "sameStep": shot["step"] == player["step"]}
	if result["sameStep"] != true {
		result["note"] = "the view and the player view drew different steps; hold time (time mode manual) to capture both at one"
	}
	return result, nil
}

// inspectionCapture saves one frames/capture of a view (or, with no view, of
// the product's composition) with a JSON sidecar holding its frame facts and
// the view and overlay headers.
func (r *runner) inspectionCapture(ctx context.Context, view string, q Request) (map[string]any, error) {
	frame, err := r.host.Capture(ctx, CaptureOptions{Width: q.Width, Height: q.Height, View: view, Overlays: q.Overlays})
	if err != nil {
		return nil, err
	}
	stem := fmt.Sprintf("player-step-%d-%s", frame.Step, uuid.NewString()[:8])
	source := "runtime frames/capture: the product's composition; world only, no product UI"
	if view != "" {
		stem = fmt.Sprintf("view-%s-step-%d-%s", view, frame.Step, uuid.NewString()[:8])
		source = "runtime frames/capture?view: inspection view " + view + "; world only, no product UI"
	}
	path := filepath.Join(r.directory, stem+".png")
	if err := evidence.WriteFileAtomic(path, frame.Payload); err != nil {
		return nil, err
	}
	result := map[string]any{"path": path, "width": frame.Width, "height": frame.Height, "step": frame.Step, "frame": frameFacts(frame),
		"overlaysRequested": q.Overlays, "frameCorrelation": "capture-step", "source": source}
	headers := map[string]string{}
	for header, value := range map[string]string{"X-Rusty-Frame-Cameras": frame.Cameras, "X-Rusty-Frame-View": frame.View, "X-Rusty-Frame-Overlays": frame.Overlays} {
		if value != "" {
			headers[header] = value
		}
	}
	result["headers"] = headers
	if view != "" {
		result["viewId"] = view
	}
	metadata := filepath.Join(r.directory, stem+".json")
	result["metadataPath"] = metadata
	return result, evidence.WriteJSONFile(metadata, result)
}
