package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"crew-services/internal/playtest/evidence"
	"crew-services/internal/playtest/input"
	"github.com/google/uuid"
)

// Request is one assist operation, the same surface the browser lane offers.
type Request struct {
	Op             string          `json:"op"`
	Mode           string          `json:"mode,omitempty"`
	ID             string          `json:"id,omitempty"`
	MS             *float64        `json:"ms,omitempty"`
	Yaw            *float64        `json:"yaw,omitempty"`
	Pitch          *float64        `json:"pitch,omitempty"`
	Camera         json.RawMessage `json:"camera,omitempty"`
	X              *float64        `json:"x,omitempty"`
	Y              *float64        `json:"y,omitempty"`
	Z              *float64        `json:"z,omitempty"`
	Radius         *int            `json:"radius,omitempty"`
	VerticalRadius *int            `json:"verticalRadius,omitempty"`
	CellSize       *float64        `json:"cellSize,omitempty"`
	Distance       *float64        `json:"distance,omitempty"`
	Move           *[3]float64     `json:"move,omitempty"`
	LookAt         *[3]float64     `json:"lookAt,omitempty"`
	Orbit          *struct {
		Target [3]float64 `json:"target"`
		Yaw    float64    `json:"yaw"`
	} `json:"orbit,omitempty"`
	Capture bool `json:"capture,omitempty"`
	World   bool `json:"world,omitempty"`
	Count   int  `json:"count,omitempty"`
	FPS     int  `json:"fps,omitempty"`
	GIF     bool `json:"gif,omitempty"`
	Width   int  `json:"width,omitempty"`
	Height  int  `json:"height,omitempty"`
	// Steps are OS-tier input for os-input (Windows instances).
	Steps []map[string]any `json:"steps,omitempty"`
	// Desktop points window and os-input at the whole Windows desktop
	// instead of the session's instance window.
	Desktop bool `json:"desktop,omitempty"`
}

type runner struct {
	host      *Host
	directory string
	input     *claim
	windows   *windowsInstance
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (r *runner) run(ctx context.Context, q Request) (map[string]any, error) {
	if q.Op == "" {
		q.Op = "discover"
	}
	if q.ID != "" && !identifier.MatchString(q.ID) {
		return nil, errors.New("invalid target/action id")
	}
	switch q.Op {
	case "discover":
		return r.discover(ctx)
	case "observe":
		facts, err := r.host.Debug(ctx, "playtest.observe")
		if err == nil {
			facts["observationProvenance"] = map[string]any{"sampledAt": time.Now().UTC(), "kind": "live product query", "frameCorrelation": "not measured"}
		}
		return facts, err
	case "focus":
		return map[string]any{"focused": nil, "pointerLocked": nil, "note": "no page: claimed input does not depend on window focus"}, nil
	case "flush":
		return map[string]any{"flushed": true, "note": "claimed input is posted directly; the next debug command hands it to the runtime"}, nil
	case "interaction":
		return r.host.Debug(ctx, "interaction.inspect")
	case "grid":
		radius, vertical, size := intOr(q.Radius, 4), intOr(q.VerticalRadius, 4), floatOr(q.CellSize, 0.25)
		if radius < 0 || vertical < 0 || radius > 15 || vertical > 15 || size < .125 || size > 2 {
			return nil, errors.New("invalid grid dimensions; radii 0..15, cellSize .125..2")
		}
		return r.host.Debug(ctx, fmt.Sprintf("spatial.grid %d %d %g", radius, vertical, size))
	case "probe":
		distance := floatOr(q.Distance, 2)
		if !(distance > 0 && distance <= 8) {
			return nil, errors.New("probe distance must be in (0,8]")
		}
		return r.host.Debug(ctx, fmt.Sprintf("spatial.probe %g", distance))
	case "clearance", "jump-plan":
		if q.X == nil || q.Y == nil || q.Z == nil || !finite(*q.X, *q.Y, *q.Z) {
			return nil, fmt.Errorf("%s requires finite x/y/z target feet coordinates", q.Op)
		}
		command := map[string]string{"clearance": "spatial.clearance", "jump-plan": "playtest.jump-plan"}[q.Op]
		return r.host.Debug(ctx, fmt.Sprintf("%s %g %g %g", command, *q.X, *q.Y, *q.Z))
	case "action":
		return r.host.Debug(ctx, "playtest.action "+q.ID)
	case "targets":
		return r.host.Debug(ctx, "navigation.targets")
	case "route":
		if q.ID == "" {
			return nil, errors.New(`route requires id from targets; example: {op:"route",id:"door-north-wing"}`)
		}
		return r.host.Debug(ctx, "navigation.route "+q.ID)
	case "look":
		yaw, pitch := floatOr(q.Yaw, 0), floatOr(q.Pitch, 0)
		if !finite(yaw, pitch) || math.Abs(yaw) > 360 || math.Abs(pitch) > 180 {
			return nil, errors.New("look degrees exceed bounds")
		}
		return r.host.Debug(ctx, fmt.Sprintf("playtest.look %g %g", yaw, pitch))
	case "time":
		if q.Mode == "" {
			return r.host.Debug(ctx, "engine.time")
		}
		if q.Mode != "realtime" && q.Mode != "manual" && q.Mode != "action-driven" {
			return nil, errors.New("unknown time mode")
		}
		return r.host.Debug(ctx, "engine.time.mode "+q.Mode)
	case "advance":
		ms := floatOr(q.MS, 0)
		if !(ms > 0 && ms <= 2000) {
			return nil, errors.New("advance ms must be in (0, 2000]")
		}
		return r.host.Debug(ctx, fmt.Sprintf("engine.time.advance %g", ms))
	case "drawing":
		if q.Mode != "continuous" && q.Mode != "on-demand" {
			return nil, errors.New("drawing mode must be continuous or on-demand")
		}
		return r.host.Debug(ctx, "engine.renderer.drawing "+q.Mode)
	case "frame":
		if _, err := r.host.Debug(ctx, "engine.renderer.frame"); err != nil {
			return nil, err
		}
		return r.host.Debug(ctx, "engine.renderer.presentation")
	case "camera":
		return r.camera(ctx, q)
	case "world-frame", "capture":
		return r.capture(ctx, r.directory, "world", q.Width, q.Height)
	case "act":
		return r.withCapture(ctx, q, func() (map[string]any, error) { return r.act(ctx, q.ID, q.MS, nil, nil, 2000) })
	case "jump":
		return r.withCapture(ctx, q, func() (map[string]any, error) { return r.jump(ctx, q) })
	case "window":
		return r.window(ctx, q.Desktop)
	case "os-input":
		return r.osInput(ctx, q.Steps, q.Desktop)
	case "survey":
		return r.survey(ctx, q)
	case "record":
		return r.record(ctx, q)
	}
	return nil, fmt.Errorf("unknown assist operation %q", q.Op)
}

// window captures the instance window on the Windows box as Windows composes
// it: the world with the product UI and HUD over it. With desktop it captures
// the whole screen instead: other windows, dialogs and the taskbar.
func (r *runner) window(ctx context.Context, desktop bool) (map[string]any, error) {
	if r.windows == nil {
		return nil, errors.New("capability_unavailable: window capture needs a windows-desktop session")
	}
	if desktop {
		png, err := r.windows.agent.Desktop(ctx)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(r.directory, "desktop-"+uuid.NewString()[:8]+".png")
		if err := evidence.WriteFileAtomic(path, png); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "source": "Windows desktop capture: the whole screen as shown",
			"pointing": "os-input point steps with desktop:true take x/y in this image, with its width and height"}, nil
	}
	png, err := r.windows.agent.Window(ctx, r.windows.instance)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(r.directory, "window-"+uuid.NewString()[:8]+".png")
	if err := evidence.WriteFileAtomic(path, png); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "source": "Windows window capture: world and product UI as composed on the desktop",
		"frameCorrelation": "not measured; capture world-frame for a step-named image"}, nil
}

// osInput sends OS input (SendInput) to the instance window under the box's
// foreground lease: Windows focus, the desktop shell's input path, pointer
// lock and the page's input capture, as a person's keyboard and mouse would.
// With desktop it leases the desktop instead and raises no window, for
// dialogs and other windows; point steps then use desktop coordinates.
func (r *runner) osInput(ctx context.Context, steps []map[string]any, desktop bool) (map[string]any, error) {
	if r.windows == nil {
		return nil, errors.New("capability_unavailable: os-input needs a windows-desktop session")
	}
	if err := input.ValidateBatch(steps); err != nil {
		return nil, err
	}
	instance := r.windows.instance
	if desktop {
		instance = ""
	}
	receipt, err := r.windows.agent.OSInput(ctx, r.windows.holder, instance, steps)
	if receipt == nil {
		receipt = map[string]any{}
	}
	receipt["productAcceptance"] = "unavailable; observe the effect"
	return receipt, err
}

func (r *runner) discover(ctx context.Context) (map[string]any, error) {
	names, err := r.host.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, name := range names {
		has[name] = true
	}
	operations := []string{"discover", "observe", "action", "look", "time", "advance", "drawing", "frame", "camera", "targets", "route", "world-frame", "act", "survey", "record"}
	for op, command := range map[string]string{"interaction": "interaction.inspect", "grid": "spatial.grid", "probe": "spatial.probe", "clearance": "spatial.clearance", "jump-plan": "playtest.jump-plan"} {
		if has[command] {
			operations = append(operations, op)
		}
	}
	if has["playtest.jump-plan"] {
		operations = append(operations, "jump")
	}
	if r.windows != nil {
		operations = append(operations, "window", "os-input")
	}
	result := map[string]any{"backend": "engine", "commands": names, "nativeCommands": names, "operations": operations,
		"commandNote": "nativeCommands are debug catalog names, not assist operations; act/jump/survey/record are harness compositions",
		"time":        has["engine.time"], "timeModes": []string{"realtime", "manual", "action-driven"}, "drawingModes": []string{"continuous", "on-demand"},
		"observer": []string{"pose", "move", "lookAt", "orbit", "restore"}, "lookAdvancesTime": false, "inspection": true,
		"inputPath": "claimed runtime input: tests binding admission and the product's mappings, not the page's input capture (DOM focus, text entry, menus, pointer-lock shim)",
		"captures":  "runtime world frames named by simulation step; no product UI or HUD (attach a browser session for those)"}
	if has["playtest.help"] {
		if help, err := r.host.Debug(ctx, "playtest.help"); err == nil {
			result["product"] = help
		}
	}
	return result, nil
}

// capture saves one world frame as world-<step>-<id>.png.
func (r *runner) capture(ctx context.Context, directory, prefix string, width, height int) (map[string]any, error) {
	frame, err := r.host.Capture(ctx, width, height)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%s-step-%d-%s.png", prefix, frame.Step, uuid.NewString()[:8]))
	if err := evidence.WriteFileAtomic(path, frame.Payload); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "width": frame.Width, "height": frame.Height, "frame": frameFacts(frame),
		"frameCorrelation": "capture-step", "source": "runtime frames/capture; world only, no product UI"}, nil
}

func (r *runner) withCapture(ctx context.Context, q Request, act func() (map[string]any, error)) (map[string]any, error) {
	result, err := act()
	if err != nil || !q.Capture {
		return result, err
	}
	shot, captureErr := r.capture(ctx, r.directory, "action", q.Width, q.Height)
	if captureErr != nil {
		result["captureError"] = captureErr.Error()
		return result, nil
	}
	result["capture"], result["captureFrame"], result["frameCorrelation"] = shot["path"], shot["frame"], shot["frameCorrelation"]
	return result, nil
}

type actionPlan struct {
	Available  bool     `json:"available"`
	Reason     string   `json:"reason"`
	Key        string   `json:"key"`
	DurationMS float64  `json:"durationMs"`
	Hold       bool     `json:"hold"`
	HeldKeys   []string `json:"heldKeys"`
	raw        map[string]any
}

func decodeInto(value map[string]any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

type timeAnswer struct {
	Mode           string  `json:"mode"`
	FixedStepHz    float64 `json:"fixedStepHz"`
	AdvancedMS     float64 `json:"advancedMs"`
	SimulationStep string  `json:"simulationStep"`
}

func (r *runner) time(ctx context.Context, command string) (timeAnswer, error) {
	answer, err := r.host.Debug(ctx, command)
	if err != nil {
		return timeAnswer{}, err
	}
	var t timeAnswer
	return t, decodeInto(answer, &t)
}

// act presses a product action's control through the input claim and, in held
// time, advances simulation while it is held. A pressed edge is consumed by
// one fixed step; a hold stays down for the whole duration. afterAdvance runs
// after each advance for recordings.
func (r *runner) act(ctx context.Context, id string, requested *float64, resolved map[string]any, afterAdvance func(float64) error, chunk float64) (map[string]any, error) {
	raw := resolved
	if raw == nil {
		var err error
		if raw, err = r.host.Debug(ctx, "playtest.action "+id); err != nil {
			return nil, err
		}
	}
	var plan actionPlan
	if err := decodeInto(raw, &plan); err != nil {
		return nil, fmt.Errorf("action plan: %w", err)
	}
	if !plan.Available {
		return map[string]any{"accepted": false, "plan": raw, "reason": plan.Reason}, nil
	}
	ms := floatOr(requested, plan.DurationMS)
	if !(ms > 0 && ms <= 2000) {
		return nil, errors.New("action duration must be in (0, 2000] ms")
	}
	if len(plan.HeldKeys) > 4 {
		return nil, errors.New("invalid product held controls")
	}
	main, err := controlFact(plan.Key)
	if err != nil {
		return nil, err
	}
	var additional []fact
	for _, key := range plan.HeldKeys {
		if key == plan.Key {
			continue
		}
		f, err := controlFact(key)
		if err != nil {
			return nil, err
		}
		additional = append(additional, f)
	}
	clock, err := r.time(ctx, "engine.time")
	if err != nil {
		return nil, err
	}
	before, _ := r.host.Debug(ctx, "playtest.observe")
	var deliveries []Delivery
	var failure, releaseError string
	advanced := 0.0
	mainReleased := false
	send := func(facts []fact) error {
		receipt, err := r.input.send(ctx, facts)
		deliveries = append(deliveries, receipt)
		return err
	}
	advance := func(step float64) error {
		answer, err := r.time(ctx, fmt.Sprintf("engine.time.advance %g", step))
		if err != nil {
			return err
		}
		advanced += answer.AdvancedMS
		if afterAdvance != nil {
			return afterAdvance(advanced)
		}
		return nil
	}
	func() {
		if err := send(append(append([]fact{}, additional...), main)); err != nil {
			failure = err.Error()
			return
		}
		if clock.Mode == "realtime" {
			if !plan.Hold {
				if err := send([]fact{released(main)}); err != nil {
					failure = err.Error()
					return
				}
				mainReleased = true
			}
			select {
			case <-ctx.Done():
				failure = ctx.Err().Error()
			case <-time.After(time.Duration(ms * float64(time.Millisecond))):
			}
			return
		}
		// The first fixed step consumes the pressed edge.
		if err := advance(math.Min(ms, 1000/clock.FixedStepHz)); err != nil {
			failure = err.Error()
			return
		}
		if !plan.Hold {
			if err := send([]fact{released(main)}); err != nil {
				failure = err.Error()
				return
			}
			mainReleased = true
		}
		for ms > advanced+0.001 {
			if err := advance(math.Min(chunk, ms-advanced)); err != nil {
				failure = err.Error()
				return
			}
		}
	}()
	lift := []fact{}
	for _, f := range additional {
		lift = append(lift, released(f))
	}
	if !mainReleased {
		lift = append(lift, released(main))
	}
	switch {
	case len(lift) == 0:
	case r.input.uncertain != "":
		// A fresh claim on the next input clears whatever is held; nothing
		// uncertain is ever sent twice.
		releaseError = "not sent: " + r.input.uncertain + "; the next claim clears held input"
	default:
		if err := send(lift); err != nil {
			releaseError = err.Error()
		}
	}
	// In held time queued input reaches the runtime with the next debug
	// command, so admission is read after the closing observation.
	after, observationErr := r.host.Debug(ctx, "playtest.observe")
	admission := r.input.admitted(ctx, time.Second)
	delta := differences(before, after)
	result := map[string]any{"accepted": failure == "" && releaseError == "", "inputPath": "claimed runtime input", "key": plan.Key, "plan": raw,
		"requestedMs": ms, "advancedMs": advanced, "inputReleased": releaseError == "", "observation": after, "delta": delta,
		"deliveries": deliveries, "admission": admission, "productAcceptance": "unavailable; inspect observed effect",
		"delivery": "submitted"}
	if failure != "" {
		result["error"], result["delivery"] = failure, "uncertain; reobserve without replay"
	}
	if releaseError != "" {
		result["releaseError"] = releaseError
	}
	if observationErr != nil {
		result["observationError"] = observationErr.Error()
	}
	if player, _ := after["player"].(map[string]any); player != nil && player["dead"] == true {
		result["handback"] = "player-dead"
	} else if moved, ok := delta["distanceMoved"].(float64); plan.Hold && ok && moved == 0 {
		result["handback"] = "no-observed-movement; inspect collision or the action's control"
	}
	return result, nil
}

func (r *runner) jump(ctx context.Context, q Request) (map[string]any, error) {
	clock, err := r.time(ctx, "engine.time")
	if err != nil {
		return nil, err
	}
	if clock.Mode == "realtime" {
		return nil, errors.New("jump helper requires manual or action-driven time")
	}
	if q.X == nil || q.Y == nil || q.Z == nil || !finite(*q.X, *q.Y, *q.Z) {
		return nil, errors.New("jump requires finite x/y/z target feet coordinates")
	}
	before, _ := r.host.Debug(ctx, "playtest.observe")
	query := fmt.Sprintf("playtest.jump-plan %g %g %g", *q.X, *q.Y, *q.Z)
	plan, err := r.host.Debug(ctx, query)
	if err != nil {
		return nil, err
	}
	if plan["available"] != true {
		return map[string]any{"accepted": false, "plan": plan, "reason": plan["reason"]}, nil
	}
	if yaw, ok := plan["yawDeltaDegrees"].(float64); ok {
		if _, err := r.host.Debug(ctx, fmt.Sprintf("playtest.look %g 0", yaw)); err != nil {
			return nil, err
		}
	}
	// Resolve against the turned product state.
	if plan, err = r.host.Debug(ctx, query); err != nil {
		return nil, err
	}
	if plan["available"] != true {
		return map[string]any{"accepted": false, "plan": plan, "reason": plan["reason"]}, nil
	}
	action, _ := plan["action"].(map[string]any)
	if action == nil {
		return nil, errors.New("jump plan has no action")
	}
	result, err := r.act(ctx, fmt.Sprint(action["id"]), nil, action, nil, 100)
	if err != nil {
		return nil, err
	}
	if settle, ok := plan["settleMs"].(float64); ok && settle > 0 && result["accepted"] == true {
		if answer, err := r.time(ctx, fmt.Sprintf("engine.time.advance %g", settle)); err != nil {
			result["accepted"], result["error"], result["delivery"] = false, err.Error(), "uncertain; reobserve without replay"
		} else {
			result["advancedMs"] = result["advancedMs"].(float64) + answer.AdvancedMS
		}
	}
	after, err := r.host.Debug(ctx, "playtest.observe")
	if err != nil {
		result["observationError"] = err.Error()
	}
	result["observation"], result["delta"] = after, differences(before, after)
	result["jumpPlan"], result["targetFeet"] = plan, []float64{*q.X, *q.Y, *q.Z}
	result["outcome"] = "inspect actual pose; estimated input window does not guarantee landing"
	if player, _ := after["player"].(map[string]any); player != nil {
		if movement, _ := player["movement"].(map[string]any); movement != nil {
			result["grounded"] = movement["grounded"]
		}
		axes, _ := after["axes"].(map[string]any)
		position, _ := player["position"].(map[string]any)
		feetY, ok := axes["floorY"].(float64)
		if position != nil && ok {
			px, _ := position["x"].(float64)
			pz, _ := position["z"].(float64)
			result["distanceToTargetFeet"] = math.Sqrt(sq(px-*q.X) + sq(feetY-*q.Y) + sq(pz-*q.Z))
		}
	}
	return result, nil
}

type cameraPose struct {
	Position     [3]float64 `json:"position"`
	YawDegrees   float64    `json:"yawDegrees"`
	PitchDegrees float64    `json:"pitchDegrees"`
}

func (r *runner) setCamera(ctx context.Context, pose *cameraPose) (map[string]any, error) {
	if pose == nil {
		return r.host.Debug(ctx, "engine.renderer.camera none")
	}
	if !finite(pose.Position[0], pose.Position[1], pose.Position[2], pose.YawDegrees, pose.PitchDegrees) {
		return nil, errors.New("camera must be finite")
	}
	return r.host.Debug(ctx, fmt.Sprintf("engine.renderer.camera %g %g %g %g %g", pose.Position[0], pose.Position[1], pose.Position[2], pose.YawDegrees, pose.PitchDegrees))
}

func (r *runner) currentCamera(ctx context.Context) (*cameraPose, map[string]any, error) {
	answer, err := r.host.Debug(ctx, "engine.renderer.camera")
	if err != nil {
		return nil, nil, err
	}
	camera, _ := answer["camera"].(map[string]any)
	if camera == nil {
		return nil, answer, nil
	}
	var pose cameraPose
	if err := decodeInto(camera, &pose); err != nil {
		return nil, answer, err
	}
	return &pose, answer, nil
}

// camera reads the observer, sets an absolute pose, or turns a relative move,
// yaw/pitch, lookAt or orbit into one. "camera": null restores the product's.
func (r *runner) camera(ctx context.Context, q Request) (map[string]any, error) {
	var pose *cameraPose
	explicit := len(q.Camera) > 0
	if explicit && string(q.Camera) != "null" {
		pose = &cameraPose{}
		if err := json.Unmarshal(q.Camera, pose); err != nil {
			return nil, fmt.Errorf("camera: %w", err)
		}
	}
	relative := q.Move != nil || q.LookAt != nil || q.Orbit != nil || q.Yaw != nil || q.Pitch != nil
	if !relative {
		if !explicit {
			_, answer, err := r.currentCamera(ctx)
			return answer, err
		}
		return r.setCamera(ctx, pose)
	}
	if pose == nil {
		current, _, err := r.currentCamera(ctx)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, errors.New("no camera to move from; set an absolute camera")
		}
		pose = current
	}
	p := pose.Position
	if q.Move != nil {
		for i := range p {
			p[i] += q.Move[i]
		}
	}
	if q.Orbit != nil {
		x, z := p[0]-q.Orbit.Target[0], p[2]-q.Orbit.Target[2]
		angle := q.Orbit.Yaw * math.Pi / 180
		p[0], p[2] = q.Orbit.Target[0]+x*math.Cos(angle)-z*math.Sin(angle), q.Orbit.Target[2]+x*math.Sin(angle)+z*math.Cos(angle)
	}
	yaw, pitch := pose.YawDegrees+floatOr(q.Yaw, 0), pose.PitchDegrees+floatOr(q.Pitch, 0)
	target := q.LookAt
	if target == nil && q.Orbit != nil {
		target = &q.Orbit.Target
	}
	if target != nil {
		x, y, z := target[0]-p[0], target[1]-p[1], target[2]-p[2]
		yaw = math.Atan2(x, -z) * 180 / math.Pi
		pitch = math.Atan2(y, math.Hypot(x, z)) * 180 / math.Pi
	}
	return r.setCamera(ctx, &cameraPose{Position: p, YawDegrees: yaw, PitchDegrees: pitch})
}

func (r *runner) survey(ctx context.Context, q Request) (map[string]any, error) {
	count := q.Count
	if count == 0 {
		count = 4
	}
	if count != 4 && count != 8 {
		return nil, errors.New("survey count must be 4 or 8")
	}
	clock, err := r.time(ctx, "engine.time")
	if err != nil {
		return nil, err
	}
	if clock.Mode == "realtime" {
		return nil, errors.New("select manual or action-driven time before a frozen survey")
	}
	initial, answer, err := r.currentCamera(ctx)
	if err != nil {
		return nil, err
	}
	if initial == nil {
		return nil, errors.New("no camera to survey from")
	}
	observer := answer["observer"] == true
	directory := filepath.Join(r.directory, "survey-"+uuid.NewString())
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, err
	}
	var frames []map[string]any
	var failure string
	for i := 0; i < count && failure == ""; i++ {
		pose := *initial
		pose.YawDegrees += float64(i) * 360 / float64(count)
		if _, err := r.setCamera(ctx, &pose); err != nil {
			failure = err.Error()
			break
		}
		shot, err := r.capture(ctx, directory, fmt.Sprint(i), q.Width, q.Height)
		if err != nil {
			failure = err.Error()
			break
		}
		shot["relativeYawDegrees"], shot["camera"] = float64(i)*360/float64(count), pose
		frames = append(frames, shot)
	}
	restore := initial
	if !observer {
		restore = nil
	}
	_, restoreErr := r.setCamera(ctx, restore)
	if restoreErr != nil {
		failure += " restore: " + restoreErr.Error()
	}
	var paths []string
	for _, f := range frames {
		paths = append(paths, f["path"].(string))
	}
	contact, encodingErr := tile(ctx, paths, filepath.Join(directory, "contact.png"), map[int]string{4: "2x2", 8: "4x2"}[count])
	result := map[string]any{"directory": directory, "frames": frames, "contact": contact, "restored": restoreErr == nil, "time": clock}
	if failure != "" {
		result["error"] = failure
	}
	if encodingErr != nil {
		result["encodingError"] = encodingErr.Error()
	}
	return result, evidence.WriteJSONFile(filepath.Join(directory, "survey.json"), result)
}

func (r *runner) record(ctx context.Context, q Request) (map[string]any, error) {
	ms, fps := floatOr(q.MS, 1000), q.FPS
	if fps == 0 {
		fps = 10
	}
	if ms < 100 || ms > 10000 || ms != math.Trunc(ms) || fps < 1 || fps > 30 {
		return nil, errors.New("record requires ms 100..10000 and fps 1..30")
	}
	clock, err := r.time(ctx, "engine.time")
	if err != nil {
		return nil, err
	}
	if clock.Mode == "realtime" {
		return nil, errors.New("record needs manual or action-driven time: each frame is captured at an exact step")
	}
	directory := filepath.Join(r.directory, "record-"+uuid.NewString())
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, err
	}
	var frames []map[string]any
	advanced := 0.0
	capture := func() error {
		shot, err := r.capture(ctx, directory, fmt.Sprintf("%05d", len(frames)), q.Width, q.Height)
		if err != nil {
			return err
		}
		shot["advancedMs"] = advanced
		frames = append(frames, shot)
		return nil
	}
	var failure string
	var actionResult map[string]any
	interval := 1000 / float64(fps)
	if err := capture(); err != nil { // Arm before the action.
		return nil, err
	}
	func() {
		if q.ID != "" {
			span := math.Min(ms, 2000)
			actionResult, err = r.act(ctx, q.ID, &span, nil, func(elapsed float64) error { advanced = elapsed; return capture() }, interval)
			if err != nil {
				failure = err.Error()
				return
			}
			if message, _ := actionResult["error"].(string); message != "" {
				failure = message
				return
			}
		}
		for advanced < ms-0.001 {
			answer, err := r.time(ctx, fmt.Sprintf("engine.time.advance %g", math.Min(interval, ms-advanced)))
			if err != nil {
				failure = err.Error()
				return
			}
			advanced += answer.AdvancedMS
			if err := capture(); err != nil {
				failure = err.Error()
				return
			}
		}
	}()
	var paths []string
	for _, f := range frames {
		paths = append(paths, f["path"].(string))
	}
	video, contact, encodingErr := encode(ctx, paths, directory, fps, q.GIF)
	result := map[string]any{"directory": directory, "frames": frames, "video": video, "contact": contact, "time": clock,
		"requestedMs": ms, "advancedMs": advanced, "nominalFps": fps, "actionResult": actionResult}
	if failure != "" {
		result["error"] = failure
	}
	if encodingErr != nil {
		result["encodingError"] = encodingErr.Error()
	}
	if q.GIF && encodingErr == nil {
		result["gif"] = filepath.Join(directory, "clip.gif")
	}
	return result, evidence.WriteJSONFile(filepath.Join(directory, "record.json"), result)
}

// sequence links frames as 00000.png.. so ffmpeg reads them in order.
func sequence(paths []string, directory string) (string, error) {
	for i, path := range paths {
		if err := os.Link(path, filepath.Join(directory, fmt.Sprintf("seq-%05d.png", i))); err != nil {
			return "", err
		}
	}
	return filepath.Join(directory, "seq-%05d.png"), nil
}

func ffmpeg(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ffmpeg", append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, output)
	}
	return nil
}

func tile(ctx context.Context, paths []string, out, layout string) (any, error) {
	if len(paths) == 0 {
		return nil, errors.New("no frames")
	}
	pattern, err := sequence(paths, filepath.Dir(out))
	if err != nil {
		return nil, err
	}
	if err := ffmpeg(ctx, "-i", pattern, "-vf", "scale=320:-1,tile="+layout, "-frames:v", "1", out); err != nil {
		return nil, err
	}
	return out, nil
}

func encode(ctx context.Context, paths []string, directory string, fps int, gif bool) (any, any, error) {
	if len(paths) == 0 {
		return nil, nil, errors.New("no frames")
	}
	pattern, err := sequence(paths, directory)
	if err != nil {
		return nil, nil, err
	}
	video, contact := filepath.Join(directory, "clip.mp4"), filepath.Join(directory, "contact.png")
	if err := ffmpeg(ctx, "-framerate", fmt.Sprint(fps), "-i", pattern, "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", "-c:v", "libx264", "-pix_fmt", "yuv420p", video); err != nil {
		return nil, nil, err
	}
	every := (len(paths) + 15) / 16
	if err := ffmpeg(ctx, "-i", pattern, "-vf", fmt.Sprintf(`select=not(mod(n\,%d)),scale=320:-1,tile=4x4`, max(1, every)), "-frames:v", "1", contact); err != nil {
		return video, nil, err
	}
	if gif {
		if err := ffmpeg(ctx, "-i", video, "-vf", "fps=10,scale=640:-1", filepath.Join(directory, "clip.gif")); err != nil {
			return video, contact, err
		}
	}
	return video, contact, nil
}

// differences reports compact before/after product facts; absent facts stay
// unavailable and never become a success claim.
func differences(before, after map[string]any) map[string]any {
	a, _ := before["player"].(map[string]any)
	b, _ := after["player"].(map[string]any)
	if a == nil || b == nil {
		return map[string]any{"available": false}
	}
	result := map[string]any{}
	for _, key := range []string{"health", "kills"} {
		x, ok1 := a[key].(float64)
		y, ok2 := b[key].(float64)
		if ok1 && ok2 {
			result[key] = y - x
		}
	}
	pa, _ := a["position"].(map[string]any)
	pb, _ := b["position"].(map[string]any)
	if pa != nil && pb != nil {
		sum := 0.0
		for _, axis := range []string{"x", "y", "z"} {
			x, _ := pa[axis].(float64)
			y, _ := pb[axis].(float64)
			sum += sq(y - x)
		}
		result["distanceMoved"] = math.Sqrt(sum)
	}
	aa, _ := a["ammo"].(map[string]any)
	ab, _ := b["ammo"].(map[string]any)
	if aa != nil && ab != nil {
		ammo := map[string]any{}
		for key, value := range ab {
			y, ok1 := value.(float64)
			x, ok2 := aa[key].(float64)
			if ok1 && ok2 {
				ammo[key] = y - x
			}
		}
		result["ammo"] = ammo
	}
	return result
}

func sq(v float64) float64 { return v * v }

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func floatOr(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func intOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
