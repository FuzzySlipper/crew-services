package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crew-services/internal/playtest/evidence"
	"crew-services/internal/playtest/session"
	"github.com/google/uuid"
)

func capabilities() map[string]any {
	return map[string]any{
		"engine_queries":   true,
		"keyboard":         true,
		"relative_mouse":   true,
		"pointer_buttons":  true,
		"gamepad":          true,
		"frame_capture":    "runtime frames/capture: lossless world frame named by its simulation step; no product UI",
		"browser_inspect":  false,
		"dom_actions":      false,
		"absolute_pointer": false,
		"input_layers":     "claimed runtime input: binding admission and product mappings; not page input capture, DOM focus, text entry, menus or the pointer-lock shim",
	}
}

// Config names the adapter's private state directory.
type Config struct {
	State string
	// Label names this harness on the product's claim banner.
	Label string
}

// Adapter owns one session's connection to a product host at a time. It holds
// no process: the product host belongs to the session's launcher.
type Adapter struct {
	config Config

	mu          sync.Mutex
	opMu        sync.Mutex
	profile     session.Profile
	leaseID     string
	directory   string
	host        *Host
	input       *claim
	unavailable string
	closed      bool
}

func New(config Config) (*Adapter, error) {
	if !filepath.IsAbs(config.State) {
		return nil, fmt.Errorf("engine state directory must be absolute, got %q", config.State)
	}
	if err := os.MkdirAll(config.State, 0o700); err != nil {
		return nil, fmt.Errorf("create engine state directory: %w", err)
	}
	if config.Label == "" {
		config.Label = "crew-playtest"
	}
	return &Adapter{config: config}, nil
}

func (a *Adapter) SelectProfile(profile session.Profile) error {
	if profile.Host == nil && strings.TrimSpace(profile.URL) == "" {
		return errors.New("engine profile needs a host or a url")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID != "" {
		return errors.New("release the existing engine lease before selecting another profile")
	}
	a.profile = profile
	return nil
}

// Acquire reserves the adapter and its evidence directory. Viewport and FPS
// belong to page viewers; captures choose their own size.
func (a *Adapter) Acquire(_ context.Context, _, _, _, ttl int) (map[string]any, error) {
	if ttl < 1 {
		return nil, errors.New("engine acquisition needs a positive TTL")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.closed:
		return nil, errors.New("engine adapter is closed")
	case a.leaseID != "":
		return nil, errors.New("release the existing engine lease before acquiring another")
	case a.profile.ID == "":
		return nil, errors.New("select an engine profile before acquiring")
	}
	leaseID := uuid.NewString()
	directory := filepath.Join(a.config.State, leaseID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create engine lease directory: %w", err)
	}
	a.leaseID, a.directory, a.host, a.input, a.unavailable = leaseID, directory, nil, nil, ""
	return map[string]any{"lease_id": leaseID, "artifact_directory": directory, "events_path": filepath.Join(directory, "events.jsonl"),
		"game_readiness": "unknown", "capabilities": capabilities()}, nil
}

// Launch connects to the session's product host and reads its time and
// runtime binding. Hosted profiles arrive with their session URL set.
func (a *Adapter) Launch(ctx context.Context, leaseID string, profile session.Profile) (map[string]any, error) {
	a.mu.Lock()
	if a.leaseID != leaseID {
		a.mu.Unlock()
		return nil, errors.New("this adapter does not own that engine lease")
	}
	sameHost := (a.profile.Host == nil) == (profile.Host == nil) && (a.profile.Host == nil || *a.profile.Host == *profile.Host)
	if profile.ID != a.profile.ID || !sameHost || (profile.Host == nil && profile.URL != a.profile.URL) {
		a.mu.Unlock()
		return nil, errors.New("engine profile changed after acquisition")
	}
	host, err := NewHost(profile.URL)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.profile.URL, a.host = profile.URL, host
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var answer map[string]any
	for {
		answer, err = host.Debug(ctx, "engine.time")
		if !errors.Is(err, ErrUnavailable) || ctx.Err() != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("engine host is not answering live-debug: %w", err)
	}
	return map[string]any{"origin": host.Origin, "server_http_ready": true, "time": answer, "game_readiness": "unknown; observe or capture",
		"capabilities": capabilities(), "events_path": filepath.Join(a.directory, "events.jsonl")}, nil
}

func (a *Adapter) owned(leaseID string) (*Host, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.leaseID != leaseID || leaseID == "":
		return nil, "", errors.New("this adapter does not own that engine lease")
	case a.unavailable != "":
		return nil, "", fmt.Errorf("engine lease unavailable: %s; stop or recover explicitly", a.unavailable)
	case a.host == nil:
		return nil, "", errors.New("engine lease has no launched host")
	}
	return a.host, a.directory, nil
}

// Observe captures the runtime's world frame as an original PNG with a JSON
// sidecar. The frame is named by the simulation step it shows.
func (a *Adapter) Observe(ctx context.Context, leaseID string) (map[string]any, error) {
	host, directory, err := a.owned(leaseID)
	if err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	frame, err := host.Capture(ctx, 0, 0)
	ended := time.Now().UTC()
	if err != nil {
		return nil, err
	}
	artifactID := uuid.NewString()
	path := filepath.Join(directory, artifactID+".png")
	if err := evidence.WriteFileAtomic(path, frame.Payload); err != nil {
		return nil, fmt.Errorf("persist engine capture: %w", err)
	}
	result := map[string]any{"event": "observation", "lease_id": leaseID, "artifact_id": artifactID, "path": path,
		"events_path": filepath.Join(directory, "events.jsonl"), "width": frame.Width, "height": frame.Height,
		"capture_started_at": started, "capture_ended_at": ended, "source": "runtime frames/capture: world frame only, no product UI or HUD",
		"frame": frameFacts(frame), "frame_correlation": "capture-step", "game_readiness": "unknown", "frame_freshness": "drawn for this capture"}
	metadataPath := filepath.Join(directory, artifactID+".json")
	result["metadata_path"] = metadataPath
	if err := evidence.WriteJSONFile(metadataPath, result); err != nil {
		return result, fmt.Errorf("persist engine observation metadata: %w", err)
	}
	return result, nil
}

func frameFacts(frame Frame) map[string]any {
	facts := map[string]any{"captureSequence": frame.Sequence, "step": frame.Step, "held": frame.Held, "video": frame.Video}
	if frame.Cameras != "" {
		var cameras any
		if json.Unmarshal([]byte(frame.Cameras), &cameras) == nil {
			facts["cameras"] = cameras
		} else {
			facts["cameras"] = frame.Cameras
		}
	}
	return facts
}

// Browser serves assist operations. DOM operations need a page.
func (a *Adapter) Browser(ctx context.Context, leaseID string, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return nil, errors.New("assist data must be a JSON object up to 65536 bytes")
	}
	var request struct {
		Op      string          `json:"op"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("assist data must be a JSON object")
	}
	if request.Op != "playtest" {
		return nil, fmt.Errorf("capability_unavailable: browser op %q needs a page; the engine backend has no DOM", request.Op)
	}
	host, directory, err := a.owned(leaseID)
	if err != nil {
		return nil, err
	}
	var op Request
	if len(request.Request) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(request.Request)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&op); err != nil {
			return nil, fmt.Errorf("assist request: %w", err)
		}
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return (&runner{host: host, directory: directory, input: a.claimFor(host)}).run(ctx, op)
}

// Input delivers raw hold/move/click/wait/gamepad steps under the harness's
// input claim, in wall time. Use assist act or advance for held time.
func (a *Adapter) Input(ctx context.Context, leaseID string, steps []map[string]any) (map[string]any, error) {
	host, _, err := a.owned(leaseID)
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.claimFor(host).steps(ctx, steps)
}

func (a *Adapter) claimFor(host *Host) *claim {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.input == nil || a.input.host != host {
		a.input = &claim{host: host, label: a.config.Label}
	}
	return a.input
}

// Cancel has no in-flight process to stop: each operation is bounded HTTP.
// Held input is released by the next release or by the claim's lease.
func (a *Adapter) Cancel(_ context.Context, leaseID string) (map[string]any, error) {
	if _, _, err := a.owned(leaseID); err != nil {
		return nil, err
	}
	return map[string]any{"cancelled": false, "lease_state": "active", "note": "engine operations are bounded requests; nothing is running between them"}, nil
}

func (a *Adapter) Status(_ context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if leaseID == "" {
		leaseID = a.leaseID
	}
	if leaseID == "" {
		return map[string]any{}, nil
	}
	if a.leaseID != leaseID {
		return nil, errors.New("this adapter does not own that engine lease")
	}
	state := map[string]any{"id": leaseID, "state": "active"}
	if a.unavailable != "" {
		state["state"], state["reason"] = "unavailable", a.unavailable
	}
	result := map[string]any{"lease": state, "capabilities": capabilities()}
	if a.host != nil {
		result["origin"] = a.host.Origin
	}
	if a.input != nil {
		result["input_claim"] = a.input.status()
	}
	return result, nil
}

// Release returns input to any page by releasing the claim, then forgets the
// lease. The product host itself is stopped by the session's launcher.
func (a *Adapter) Release(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if a.leaseID == "" {
		a.mu.Unlock()
		return map[string]any{"released": true}, nil
	}
	if a.leaseID != leaseID {
		a.mu.Unlock()
		return nil, errors.New("this adapter does not own that engine lease")
	}
	input, directory := a.input, a.directory
	a.mu.Unlock()
	receipt := map[string]any{"released": true, "artifact_directory": directory}
	if input != nil {
		a.opMu.Lock()
		claimReceipt, err := input.release(ctx)
		a.opMu.Unlock()
		receipt["input_claim"] = claimReceipt
		if err != nil {
			// The lease lapses on its own; a failed release never replays input.
			receipt["input_claim_error"] = err.Error()
		}
	}
	a.mu.Lock()
	if a.leaseID == leaseID {
		a.leaseID, a.directory, a.host, a.input, a.unavailable = "", "", nil, nil, ""
	}
	a.mu.Unlock()
	return receipt, nil
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	leaseID := a.leaseID
	a.mu.Unlock()
	if leaseID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.Release(ctx, leaseID)
	return err
}
