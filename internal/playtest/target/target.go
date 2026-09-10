// Package target implements a local, single-lease Wolf input target.
package target

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	defaultPort = 48190
	stateLease  = "lease.json"
)

var inputHWDBName = regexp.MustCompile(`^c13:([0-9]+)$`)

var (
	inputReadyTimeout = 3 * time.Second
	inputReadyPoll    = 50 * time.Millisecond
)

// Wolf is the small subset of Wolf's HTTP API needed by a playtest target.
// Responses deliberately stay untyped because Wolf owns their schema.
type Wolf interface {
	Call(context.Context, string, any) (map[string]any, error)
}

// UnixWolf calls Wolf over its standard Unix HTTP socket.
type UnixWolf struct{ socket string }

func NewUnixWolf(socket string) *UnixWolf { return &UnixWolf{socket: socket} }

func (w *UnixWolf) Call(ctx context.Context, route string, data any) (map[string]any, error) {
	method := http.MethodGet
	var body *bytes.Reader
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		method, body = http.MethodPost, bytes.NewReader(encoded)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://wolf/api/v1/"+route, body)
	if err != nil {
		return nil, err
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.socket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	result := map[string]any{}
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || result["success"] != true {
		if problem, ok := result["error"]; ok {
			return nil, fmt.Errorf("Wolf %s: %v", route, problem)
		}
		return nil, fmt.Errorf("Wolf %s: %s", route, resp.Status)
	}
	return result, nil
}

type Lease struct {
	ID                string   `json:"id"`
	ClientID          string   `json:"client_id"`
	TTLSeconds        int      `json:"ttl_seconds"`
	ExpiresAt         float64  `json:"expires_at"`
	BaselineLobbies   []string `json:"baseline_lobbies"`
	Lobbies           []string `json:"lobbies"`
	Gamepad           bool     `json:"gamepad,omitempty"`
	FirefoxLobbyID    string   `json:"firefox_lobby_id,omitempty"`
	FirefoxLaunchName string   `json:"firefox_launch_name,omitempty"`
	Closing           bool     `json:"closing,omitempty"`
}

// Controller owns target-local lease state. It does not expose stream secrets.
type Controller struct {
	wolf                    Wolf
	state, clientID         string
	runnerStateFolder       string
	runnerContainerName     string
	wolfStateDir            string
	inputMounts             func(string, string) ([]string, error)
	clearInputState         func(string, string) error
	mu                      sync.Mutex
	actionMu                sync.Mutex
	cancel                  chan struct{}
	activeAction            string
	cancelled               map[string]struct{}
	lease                   *Lease
	lastRelease             map[string]any
	lastInput               map[string]any
	videoProducerBufferCaps string
	now                     func() time.Time
}

// SetVideoProducerBufferCaps configures the runner buffer caps for this target.
// It is deliberately target configuration rather than a generic Wolf default:
// GPU capabilities are host-specific and are not exposed by GET /profiles.
func (c *Controller) SetVideoProducerBufferCaps(caps string) {
	c.mu.Lock()
	c.videoProducerBufferCaps = strings.TrimSpace(caps)
	c.mu.Unlock()
}

// SetWolfStateDir enables per-session Firefox input isolation using Wolf's host state.
// The directory is mounted into Wolf itself and is never exposed through the command API.
func (c *Controller) SetWolfStateDir(stateDir string) error {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		c.mu.Lock()
		c.wolfStateDir = ""
		c.mu.Unlock()
		return nil
	}
	if !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return errors.New("Wolf state directory must be a clean absolute path")
	}
	c.mu.Lock()
	c.wolfStateDir = stateDir
	c.mu.Unlock()
	return nil
}

// SetBrowserRunner configures the target-local state and Docker identity used
// for a Firefox launch. Empty values retain the installed Wolf profile's
// existing names, which preserves the original single-target deployment.
func (c *Controller) SetBrowserRunner(stateFolder, containerName string) error {
	stateFolder = strings.TrimSpace(stateFolder)
	containerName = strings.TrimSpace(containerName)
	if stateFolder != "" && (!strings.HasPrefix(stateFolder, "playtest/user/") || strings.Contains(stateFolder, "..")) {
		return errors.New("runner state folder must be below playtest/user")
	}
	if containerName != "" && strings.ContainsAny(containerName, "/\\") {
		return errors.New("runner container name must not contain a path separator")
	}
	c.mu.Lock()
	c.runnerStateFolder = stateFolder
	c.runnerContainerName = containerName
	c.mu.Unlock()
	return nil
}

func NewController(wolf Wolf, state, clientID string) (*Controller, error) {
	if err := os.MkdirAll(state, 0o750); err != nil {
		return nil, err
	}
	c := &Controller{wolf: wolf, state: state, clientID: clientID, cancelled: map[string]struct{}{}, now: time.Now, inputMounts: resolveInputMounts, clearInputState: clearStaleJoystickState}
	data, err := os.ReadFile(filepath.Join(state, stateLease))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) > 0 && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		var lease Lease
		if err := json.Unmarshal(data, &lease); err != nil {
			return nil, fmt.Errorf("read persisted lease: %w", err)
		}
		lease.ExpiresAt = 0 // A process restart never resumes unattended input.
		c.lease = &lease
	}
	return c, nil
}

func (c *Controller) saveLocked() error {
	data, err := json.Marshal(c.lease)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.state, "lease-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(c.state, stateLease))
}

func (c *Controller) record(event map[string]any) error {
	event["time"] = c.now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.state, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	return err
}

func asSlice(value any, name string) ([]any, error) {
	v, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	return v, nil
}

func object(value any, name string) (map[string]any, error) {
	v, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return v, nil
}

func integer(value any, low, high int, name string) (int, error) {
	var v int64
	switch n := value.(type) {
	case int:
		v = int64(n)
	case int64:
		v = n
	case json.Number:
		var err error
		v, err = n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer in [%d, %d]", name, low, high)
		}
	default:
		return 0, fmt.Errorf("%s must be an integer in [%d, %d]", name, low, high)
	}
	if v < int64(low) || v > int64(high) {
		return 0, fmt.Errorf("%s must be an integer in [%d, %d]", name, low, high)
	}
	return int(v), nil
}

func number(value any, low float64, name string) (float64, error) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s must be finite in [%g, 1]", name, low)
		}
	default:
		return 0, fmt.Errorf("%s must be finite in [%g, 1]", name, low)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < low || n > 1 {
		return 0, fmt.Errorf("%s must be finite in [%g, 1]", name, low)
	}
	return n, nil
}

// This Wolf virtual device's packet-to-evdev layout drives BTN_WEST (Xbox X)
// from 0x8000 and BTN_NORTH (Xbox Y) from 0x4000. Keep that device-specific
// translation at the packet boundary so callers consistently use physical
// Xbox names and Firefox's standard Gamepad positions.
var gamepadButtons = map[string]uint16{"up": 1, "down": 2, "left": 4, "right": 8, "start": 16, "back": 32, "ls": 64, "rs": 128, "lb": 256, "rb": 512, "guide": 1024, "a": 4096, "b": 8192, "x": 32768, "y": 16384}

func packet(kind uint32, payload []byte) string {
	body := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(body, kind)
	copy(body[4:], payload)
	result := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint16(result, 0x0206)
	binary.LittleEndian.PutUint16(result[2:], uint16(len(body)+4))
	binary.BigEndian.PutUint32(result[4:], uint32(len(body)))
	copy(result[8:], body)
	return hex.EncodeToString(result)
}

func keyPacket(key int, down bool) string {
	payload := make([]byte, 6)
	// byte zero and bytes 3..5 are zero; the virtual key is little-endian.
	binary.LittleEndian.PutUint16(payload[1:], uint16(key))
	kind := uint32(4)
	if down {
		kind = 3
	}
	return packet(kind, payload)
}

func gamepadPacket(step map[string]any, connected bool) (string, error) {
	buttons := uint16(0)
	if raw, exists := step["buttons"]; exists {
		values, err := asSlice(raw, "buttons")
		if err != nil {
			return "", err
		}
		for _, value := range values {
			name, ok := value.(string)
			if !ok {
				return "", errors.New("Unknown Xbox button")
			}
			button, ok := gamepadButtons[name]
			if !ok {
				return "", errors.New("Unknown Xbox button")
			}
			buttons |= button
		}
	}
	axis := [4]float64{}
	for i, name := range []string{"lx", "ly", "rx", "ry"} {
		if value, exists := step[name]; exists {
			n, err := number(value, -1, name)
			if err != nil {
				return "", err
			}
			axis[i] = n
		}
	}
	trigger := [2]float64{}
	for i, name := range []string{"lt", "rt"} {
		if value, exists := step[name]; exists {
			n, err := number(value, 0, name)
			if err != nil {
				return "", err
			}
			trigger[i] = n
		}
	}
	payload := make([]byte, 26)
	binary.LittleEndian.PutUint16(payload[0:], 0x1a)
	if connected {
		binary.LittleEndian.PutUint16(payload[4:], 1)
	}
	binary.LittleEndian.PutUint16(payload[6:], 0x14)
	binary.LittleEndian.PutUint16(payload[8:], buttons)
	payload[10], payload[11] = byte(math.RoundToEven(trigger[0]*255)), byte(math.RoundToEven(trigger[1]*255))
	for i := range axis {
		binary.LittleEndian.PutUint16(payload[12+i*2:], uint16(int16(math.RoundToEven(axis[i]*32767))))
	}
	binary.LittleEndian.PutUint16(payload[20:], 0x9c)
	binary.LittleEndian.PutUint16(payload[24:], 0x55)
	return packet(12, payload), nil
}

func validateSteps(raw any) ([]map[string]any, error) {
	values, err := asSlice(raw, "steps")
	if err != nil || len(values) < 1 || len(values) > 100 {
		return nil, errors.New("Provide 1..100 steps")
	}
	steps := make([]map[string]any, 0, len(values))
	total := 0
	for _, value := range values {
		step, err := object(value, "step")
		if err != nil {
			return nil, err
		}
		kind, ok := step["kind"].(string)
		if !ok {
			return nil, errors.New("Unknown input kind")
		}
		if kind != "hold" && kind != "move" && kind != "point" && kind != "click" && kind != "wait" && kind != "gamepad" {
			return nil, errors.New("Unknown input kind")
		}
		if err := validateStepFields(step, kind); err != nil {
			return nil, err
		}
		ms, err := integer(defaultValue(step, "ms", 0), 0, 10000, "ms")
		if err != nil {
			return nil, err
		}
		total += ms
		if total > 10000 {
			return nil, errors.New("A batch may last at most 10000 ms")
		}
		switch kind {
		case "hold":
			keys, err := asSlice(defaultValue(step, "keys", []any{}), "keys")
			if err != nil || len(keys) < 1 || len(keys) > 8 {
				return nil, errors.New("hold needs 1..8 Windows virtual-key codes")
			}
			for _, key := range keys {
				if _, err := integer(key, 1, 255, "key"); err != nil {
					return nil, err
				}
			}
		case "move":
			for _, field := range []string{"dx", "dy"} {
				if _, err := integer(step[field], -32768, 32767, field); err != nil {
					return nil, err
				}
			}
		case "point":
			width, err := integer(step["width"], 1, 16384, "width")
			if err != nil {
				return nil, err
			}
			height, err := integer(step["height"], 1, 16384, "height")
			if err != nil {
				return nil, err
			}
			if _, err = integer(step["x"], 0, width-1, "x"); err != nil {
				return nil, err
			}
			if _, err = integer(step["y"], 0, height-1, "y"); err != nil {
				return nil, err
			}
		case "click":
			if _, err := integer(defaultValue(step, "button", 1), 1, 3, "button"); err != nil {
				return nil, err
			}
		case "gamepad":
			if _, err := gamepadPacket(step, true); err != nil {
				return nil, err
			}
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func validateStepFields(step map[string]any, kind string) error {
	allowed := map[string]map[string]struct{}{
		"hold":    {"kind": {}, "ms": {}, "keys": {}},
		"move":    {"kind": {}, "ms": {}, "dx": {}, "dy": {}},
		"point":   {"kind": {}, "ms": {}, "x": {}, "y": {}, "width": {}, "height": {}},
		"click":   {"kind": {}, "ms": {}, "button": {}},
		"wait":    {"kind": {}, "ms": {}},
		"gamepad": {"kind": {}, "ms": {}, "buttons": {}, "lx": {}, "ly": {}, "rx": {}, "ry": {}, "lt": {}, "rt": {}},
	}[kind]
	for field := range step {
		if _, ok := allowed[field]; ok {
			continue
		}
		if kind == "gamepad" && field == "back" {
			return errors.New("unknown gamepad field back; use buttons:['back']")
		}
		return fmt.Errorf("unknown %s input field %q", kind, field)
	}
	return nil
}

func defaultValue(step map[string]any, name string, fallback any) any {
	if v, ok := step[name]; ok {
		return v
	}
	return fallback
}

func (c *Controller) snapshot(ctx context.Context) ([]map[string]any, []map[string]any, error) {
	sessionResponse, err := c.wolf.Call(ctx, "sessions", nil)
	if err != nil {
		return nil, nil, err
	}
	lobbyResponse, err := c.wolf.Call(ctx, "lobbies", nil)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := sanitizeList(sessionResponse["sessions"], []string{"client_id", "app_id", "video_width", "video_height", "video_refresh_rate"})
	if err != nil {
		return nil, nil, err
	}
	lobbies, err := sanitizeList(lobbyResponse["lobbies"], []string{"id", "name", "connected_sessions"})
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease != nil {
		owned := map[string]struct{}{}
		baseline := map[string]struct{}{}
		for _, id := range c.lease.Lobbies {
			owned[id] = struct{}{}
		}
		for _, id := range c.lease.BaselineLobbies {
			baseline[id] = struct{}{}
		}
		for _, lobby := range lobbies {
			if connected(lobby["connected_sessions"], c.clientID) {
				if _, wasBaseline := baseline[stringValue(lobby["id"])]; !wasBaseline {
					owned[stringValue(lobby["id"])] = struct{}{}
				}
			}
		}
		newLobbies := make([]string, 0, len(owned))
		for id := range owned {
			newLobbies = append(newLobbies, id)
		}
		sort.Strings(newLobbies)
		if !sameStrings(newLobbies, c.lease.Lobbies) {
			c.lease.Lobbies = newLobbies
			_ = c.saveLocked()
		}
	}
	return sessions, lobbies, nil
}

func sanitizeList(raw any, fields []string) ([]map[string]any, error) {
	values, err := asSlice(raw, "Wolf result")
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(values))
	for _, rawValue := range values {
		source, err := object(rawValue, "Wolf item")
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, field := range fields {
			if value, ok := source[field]; ok {
				out[field] = value
			}
		}
		result = append(result, out)
	}
	return result, nil
}
func stringValue(v any) string { s, _ := v.(string); return s }
func connected(raw any, clientID string) bool {
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if stringValue(value) == clientID {
			return true
		}
	}
	return false
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *Controller) requireLocked(leaseID string, renew bool) error {
	if c.lease == nil || c.lease.ID != leaseID {
		return errors.New("Unknown or released lease")
	}
	if c.lease.ExpiresAt <= float64(c.now().UnixNano())/1e9 || c.lease.Closing {
		return errors.New("Lease expired or closing; acquire a new lease after cleanup")
	}
	if renew {
		c.lease.ExpiresAt = float64(c.now().Add(time.Duration(c.lease.TTLSeconds)*time.Second).UnixNano()) / 1e9
		return c.saveLocked()
	}
	return nil
}

// Dispatch serves the stable JSON command protocol used by the Wolf adapter.
func (c *Controller) Dispatch(ctx context.Context, request map[string]any) (map[string]any, error) {
	op, _ := request["op"].(string)
	switch op {
	case "input":
		return c.input(ctx, stringValue(request["lease_id"]), request["steps"], stringValue(request["action_id"]))
	case "launch":
		return c.launch(ctx, stringValue(request["lease_id"]), stringValue(request["url"]))
	case "release":
		return c.release(ctx, stringValue(request["lease_id"]), "requested")
	case "acquire":
		c.mu.Lock()
		if c.lease != nil {
			c.mu.Unlock()
			return nil, errors.New("Playtest slot busy or awaiting cleanup")
		}
		c.mu.Unlock()
		ttl, err := integer(defaultValue(request, "ttl_seconds", 300), 15, 1800, "ttl_seconds")
		if err != nil {
			return nil, err
		}
		sessions, lobbies, err := c.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		if hasClient(sessions, c.clientID) {
			return nil, errors.New("Configured Moonlight identity is already streaming")
		}
		clients, err := c.wolf.Call(ctx, "clients", nil)
		if err != nil {
			return nil, err
		}
		clientList, err := sanitizeList(clients["clients"], []string{"client_id"})
		if err != nil {
			return nil, err
		}
		if !hasClient(clientList, c.clientID) {
			return nil, errors.New("Configured Moonlight identity is not paired")
		}
		c.mu.Lock()
		wolfStateDir, clearInputState := c.wolfStateDir, c.clearInputState
		c.mu.Unlock()
		// Acquire precedes the adapter's Moonlight start. Remove only this
		// identity's stale UI gamepad HWDB records so launch can bind only a
		// node published by the newly acquired stream.
		if wolfStateDir != "" {
			if err := clearInputState(wolfStateDir, c.clientID); err != nil {
				return nil, fmt.Errorf("reset isolated Firefox input readiness: %w", err)
			}
		}
		baseline := make([]string, 0, len(lobbies))
		for _, l := range lobbies {
			baseline = append(baseline, stringValue(l["id"]))
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.lease != nil {
			return nil, errors.New("Playtest slot busy or awaiting cleanup")
		}
		c.lease = &Lease{ID: uuid.NewString(), ClientID: c.clientID, TTLSeconds: ttl, ExpiresAt: float64(c.now().Add(time.Duration(ttl)*time.Second).UnixNano()) / 1e9, BaselineLobbies: baseline, Lobbies: []string{}}
		c.cancel, c.cancelled = nil, map[string]struct{}{}
		if err := c.saveLocked(); err != nil {
			return nil, err
		}
		_ = c.record(map[string]any{"event": "acquire", "lease_id": c.lease.ID})
		return leaseMap(c.lease), nil
	case "status":
		if leaseID := stringValue(request["lease_id"]); leaseID != "" {
			c.mu.Lock()
			err := c.requireLocked(leaseID, true)
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
		}
		sessions, lobbies, err := c.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return map[string]any{"lease": leaseMap(c.lease), "sessions": sessions, "lobbies": lobbies, "last_release": c.lastRelease, "last_input": c.lastInput, "target_time": c.now().UTC().Format(time.RFC3339)}, nil
	case "cancel":
		leaseID := stringValue(request["lease_id"])
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.requireLocked(leaseID, true); err != nil {
			return nil, err
		}
		actionID := stringValue(request["action_id"])
		if actionID == "" {
			actionID = c.activeAction
		}
		if actionID != "" {
			c.cancelled[actionID] = struct{}{}
			if actionID == c.activeAction && c.cancel != nil {
				close(c.cancel)
				c.cancel = nil
			}
		}
		return map[string]any{"cancel_requested": actionID != "", "action_id": actionID}, nil
	default:
		return nil, errors.New("Unknown operation")
	}
}

func leaseMap(lease *Lease) map[string]any {
	if lease == nil {
		return nil
	}
	data, _ := json.Marshal(lease)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	return result
}
func hasClient(items []map[string]any, clientID string) bool {
	for _, item := range items {
		if stringValue(item["client_id"]) == clientID {
			return true
		}
	}
	return false
}

func cloneObject(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

// launch creates the stock Firefox lobby from Wolf's own paired profile and
// joins only this lease's configured Moonlight client. The request accepts no
// runner or profile fields: those remain Wolf-owned configuration.
func (c *Controller) launch(ctx context.Context, leaseID, gameURL string) (map[string]any, error) {
	// Launch is an action as well as a lease mutation. Serializing it with input
	// prevents game packets from crossing application creation and joining.
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireLocked(leaseID, true); err != nil {
		return nil, err
	}
	if c.lease.FirefoxLobbyID != "" || c.lease.FirefoxLaunchName != "" {
		return nil, errors.New("Firefox lobby is already launched or launching for this lease")
	}
	if c.videoProducerBufferCaps == "" {
		return nil, errors.New("video producer buffer caps are not configured; set --video-producer-buffer-caps")
	}
	sessionResponse, err := c.wolf.Call(ctx, "sessions", nil)
	if err != nil {
		return nil, err
	}
	session, err := sessionForClient(sessionResponse["sessions"], c.clientID)
	if err != nil {
		return nil, err
	}
	profiles, err := c.wolf.Call(ctx, "profiles", nil)
	if err != nil {
		return nil, err
	}
	app, err := firefoxApp(profiles["profiles"])
	if err != nil {
		return nil, err
	}
	runner, err := object(app["runner"], "Firefox runner")
	if err != nil {
		return nil, err
	}
	// The profile response remains Wolf-owned. Copy it before applying the
	// target's per-slot runner identity and startup environment.
	runner = cloneObject(runner)
	if c.wolfStateDir != "" {
		mounts, err := c.inputMounts(c.wolfStateDir, c.clientID)
		if err != nil {
			return nil, fmt.Errorf("resolve isolated Firefox input: %w", err)
		}
		if err := setIsolatedInputMounts(runner, mounts); err != nil {
			return nil, err
		}
	}
	runnerName, _ := runner["name"].(string)
	if c.runnerContainerName != "" {
		runnerName = c.runnerContainerName
		runner["name"] = runnerName
	}
	if runnerName == "" {
		return nil, errors.New("Firefox runner name is required")
	}
	runnerStateFolder := "playtest/user/" + runnerName
	if c.runnerStateFolder != "" {
		runnerStateFolder = c.runnerStateFolder
	}
	compositor, startup, err := configureBrowserRunner(runner, gameURL)
	if err != nil {
		return nil, err
	}
	appName, _ := app["title"].(string)
	launchName := fmt.Sprintf("%s playtest %s", appName, leaseID)
	// Persist launch intent before create. Wolf can time out after creating the
	// lobby, and the intent gives recovery an exact, lease-local correlation key.
	c.lease.FirefoxLaunchName = launchName
	if err := c.saveLocked(); err != nil {
		c.lease.FirefoxLaunchName = ""
		return nil, fmt.Errorf("persist Firefox launch intent: %w", err)
	}
	create := map[string]any{
		"profile_id":                "user",
		"name":                      launchName,
		"icon_png_path":             app["icon_png_path"],
		"multi_user":                false,
		"stop_when_everyone_leaves": false,
		"runner_state_folder":       runnerStateFolder,
		"runner":                    runner,
		"video_settings": map[string]any{
			"width":                      session["video_width"],
			"height":                     session["video_height"],
			"refresh_rate":               session["video_refresh_rate"],
			"runner_render_node":         app["render_node"],
			"wayland_render_node":        app["render_node"],
			"video_producer_buffer_caps": c.videoProducerBufferCaps,
		},
		"audio_settings":  map[string]any{"channel_count": session["audio_channel_count"]},
		"client_settings": session["client_settings"],
	}
	created, err := c.wolf.Call(ctx, "lobbies/create", create)
	if err != nil {
		return nil, c.recoverTimedOutLaunchLocked(ctx, launchName, err)
	}
	lobbyID, _ := created["lobby_id"].(string)
	if lobbyID == "" {
		return nil, errors.New("Wolf lobbies/create did not return a lobby_id")
	}
	// The exact created ID is durable before join, so restart and expiry own
	// this one lobby even if joining fails or the process disappears.
	if !contains(c.lease.Lobbies, lobbyID) {
		c.lease.Lobbies = append(c.lease.Lobbies, lobbyID)
		sort.Strings(c.lease.Lobbies)
	}
	c.lease.FirefoxLobbyID = lobbyID
	if err := c.saveLocked(); err != nil {
		return nil, c.launchFailureLocked(ctx, lobbyID, fmt.Errorf("persist owned lobby before join: %w", err))
	}
	if _, err := c.wolf.Call(ctx, "lobbies/join", map[string]any{"lobby_id": lobbyID, "moonlight_session_id": c.clientID}); err != nil {
		return nil, c.launchFailureLocked(ctx, lobbyID, fmt.Errorf("join created lobby: %w", err))
	}
	return map[string]any{"lobby_id": lobbyID, "name": appName, "runner_container_name": runnerName, "compositor": compositor, "browser_startup": startup}, nil
}

// recoverTimedOutLaunchLocked resolves Wolf's create-timeout ambiguity using
// the durable, unique request name. It never scans or stops unrelated lobbies.
func (c *Controller) recoverTimedOutLaunchLocked(ctx context.Context, launchName string, cause error) error {
	response, err := c.wolf.Call(ctx, "lobbies", nil)
	if err != nil {
		c.lease.Closing = true
		if saveErr := c.saveLocked(); saveErr != nil {
			return fmt.Errorf("create Firefox lobby: %w; unable to query exact launch %q: %v; persist closing lease: %v", cause, launchName, err, saveErr)
		}
		return fmt.Errorf("create Firefox lobby: %w; unable to query exact launch %q: %v", cause, launchName, err)
	}
	lobbies, err := asSlice(response["lobbies"], "Wolf lobbies")
	if err != nil {
		c.lease.Closing = true
		_ = c.saveLocked()
		return fmt.Errorf("create Firefox lobby: %w; malformed lobby reconciliation: %v", cause, err)
	}
	ids := []string{}
	for _, rawLobby := range lobbies {
		lobby, err := object(rawLobby, "Wolf lobby")
		if err != nil {
			continue
		}
		if stringValue(lobby["name"]) == launchName && stringValue(lobby["id"]) != "" {
			ids = append(ids, stringValue(lobby["id"]))
		}
	}
	if len(ids) == 0 {
		c.lease.FirefoxLaunchName = ""
		if saveErr := c.saveLocked(); saveErr != nil {
			c.lease.Closing = true
			return fmt.Errorf("create Firefox lobby: %w; no matching lobby found and clear intent failed: %v", cause, saveErr)
		}
		return fmt.Errorf("create Firefox lobby: %w", cause)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !contains(c.lease.Lobbies, id) {
			c.lease.Lobbies = append(c.lease.Lobbies, id)
		}
	}
	c.lease.FirefoxLobbyID = ids[0]
	if err := c.saveLocked(); err != nil {
		c.lease.Closing = true
		return fmt.Errorf("create Firefox lobby: %w; persist reconciled lobby IDs %v: %v", cause, ids, err)
	}
	stopErrors := []string{}
	for _, id := range ids {
		if _, err := c.wolf.Call(ctx, "lobbies/stop", map[string]any{"lobby_id": id}); err != nil {
			stopErrors = append(stopErrors, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(stopErrors) > 0 {
		c.lease.Closing = true
		_ = c.saveLocked()
		return fmt.Errorf("create Firefox lobby: %w; exact lobby cleanup failed: %s", cause, strings.Join(stopErrors, "; "))
	}
	for _, id := range ids {
		c.lease.Lobbies = without(c.lease.Lobbies, id)
	}
	c.lease.FirefoxLobbyID = ""
	c.lease.FirefoxLaunchName = ""
	if err := c.saveLocked(); err != nil {
		c.lease.Closing = true
		return fmt.Errorf("create Firefox lobby: %w; exact lobby cleanup succeeded but persist cleanup failed: %v", cause, err)
	}
	return fmt.Errorf("create Firefox lobby: %w; reconciled and stopped exact lobby IDs %v", cause, ids)
}

func sessionForClient(raw any, clientID string) (map[string]any, error) {
	sessions, err := asSlice(raw, "Wolf sessions")
	if err != nil {
		return nil, err
	}
	for _, rawSession := range sessions {
		session, err := object(rawSession, "Wolf session")
		if err != nil {
			return nil, err
		}
		if stringValue(session["client_id"]) != clientID {
			continue
		}
		for _, field := range []string{"video_width", "video_height", "video_refresh_rate", "audio_channel_count", "client_settings"} {
			if _, ok := session[field]; !ok {
				return nil, fmt.Errorf("stream session is missing %s", field)
			}
		}
		return session, nil
	}
	return nil, errors.New("Stream session has not connected")
}

func firefoxApp(raw any) (map[string]any, error) {
	profiles, err := asSlice(raw, "Wolf profiles")
	if err != nil {
		return nil, err
	}
	for _, rawProfile := range profiles {
		profile, err := object(rawProfile, "Wolf profile")
		if err != nil {
			return nil, err
		}
		if stringValue(profile["id"]) != "user" {
			continue
		}
		apps, err := asSlice(profile["apps"], "Wolf profile apps")
		if err != nil {
			return nil, err
		}
		for _, rawApp := range apps {
			app, err := object(rawApp, "Wolf app")
			if err != nil {
				return nil, err
			}
			if stringValue(app["title"]) == "Firefox" {
				if _, err := object(app["runner"], "Firefox runner"); err != nil {
					return nil, err
				}
				return app, nil
			}
		}
	}
	return nil, errors.New("Wolf profile user has no Firefox app")
}

// launchFailureLocked stops only the exact lobby created by this launch. It
// does not call release because an older lease-owned lobby must remain intact.
func (c *Controller) launchFailureLocked(ctx context.Context, lobbyID string, cause error) error {
	_, stopErr := c.wolf.Call(ctx, "lobbies/stop", map[string]any{"lobby_id": lobbyID})
	if stopErr == nil {
		c.lease.Lobbies = without(c.lease.Lobbies, lobbyID)
		if c.lease.FirefoxLobbyID == lobbyID {
			c.lease.FirefoxLobbyID = ""
		}
		c.lease.FirefoxLaunchName = ""
		if err := c.saveLocked(); err != nil {
			c.lease.Closing = true
			return fmt.Errorf("%w; stop created lobby succeeded but persist cleanup failed: %v", cause, err)
		}
		return cause
	}
	c.lease.Closing = true
	if err := c.saveLocked(); err != nil {
		return fmt.Errorf("%w; stop created lobby failed: %v; persist closing lease failed: %v", cause, stopErr, err)
	}
	return fmt.Errorf("%w; stop created lobby failed: %v", cause, stopErr)
}

func without(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func (c *Controller) send(ctx context.Context, value string, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errors.New("Input delivery budget exhausted")
	}
	timeout := remaining
	if timeout > time.Second {
		timeout = time.Second
	}
	request, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := c.wolf.Call(request, "sessions/input", map[string]any{"session_id": c.clientID, "input_packet_hex": value})
	return err
}

func stepMS(step map[string]any) int {
	ms, _ := integer(defaultValue(step, "ms", 0), 0, 10000, "ms")
	return ms
}

func (c *Controller) input(ctx context.Context, leaseID string, rawSteps any, actionID string) (map[string]any, error) {
	steps, err := validateSteps(rawSteps)
	if err != nil {
		return nil, err
	}
	if actionID == "" {
		actionID = uuid.NewString()
	}
	c.mu.Lock()
	if err := c.requireLocked(leaseID, true); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if !tryLock(&c.actionMu) {
		c.mu.Unlock()
		return nil, errors.New("Input batch already running")
	}
	cancel := make(chan struct{})
	c.cancel, c.activeAction = cancel, actionID
	_, cancelledBefore := c.cancelled[actionID]
	if cancelledBefore {
		// A simultaneous cancel is serialized by c.mu. Closing here prevents
		// the post-unlock cancel path from racing a second close.
		close(cancel)
		c.cancel = nil
	}
	c.mu.Unlock()
	receipt := map[string]any{"event": "input", "lease_id": leaseID, "action_id": actionID, "started_at": c.now().UTC().Format(time.RFC3339), "backend": "wolf-input-api", "steps": steps, "completed_steps": 0, "release_errors": []string{}}
	started, deadline := c.now(), c.now().Add(10*time.Second)
	heldKeys := map[int]struct{}{}
	heldButtons := map[int]struct{}{}
	gamepadActive := false
	defer func() {
		cleanupDeadline := deadline.Add(2 * time.Second)
		releaseErrors := receipt["release_errors"].([]string)
		for key := range heldKeys {
			if err := c.send(ctx, keyPacket(key, false), cleanupDeadline); err != nil {
				releaseErrors = append(releaseErrors, err.Error())
			}
		}
		for button := range heldButtons {
			if err := c.send(ctx, packet(9, []byte{byte(button)}), cleanupDeadline); err != nil {
				releaseErrors = append(releaseErrors, err.Error())
			}
		}
		if gamepadActive {
			if neutral, err := gamepadPacket(map[string]any{}, true); err == nil {
				if err := c.send(ctx, neutral, cleanupDeadline); err != nil {
					releaseErrors = append(releaseErrors, err.Error())
				}
			}
		}
		receipt["release_errors"] = releaseErrors
		receipt["cancelled"] = isClosed(cancel)
		receipt["ended_at"] = c.now().UTC().Format(time.RFC3339)
		receipt["elapsed_ms"] = c.now().Sub(started).Milliseconds()
		c.mu.Lock()
		c.activeAction = ""
		c.cancel = nil
		delete(c.cancelled, actionID)
		c.lastInput = receipt
		if len(releaseErrors) > 0 && c.lease != nil {
			c.lease.Closing = true
			if err := c.saveLocked(); err != nil {
				appendEvidenceError(receipt, fmt.Errorf("persist closing lease: %w", err))
			}
		}
		if err := c.record(receipt); err != nil {
			appendEvidenceError(receipt, fmt.Errorf("record input receipt: %w", err))
		}
		c.mu.Unlock()

		// Keep the action slot through neutralization and receipt persistence so
		// a following input cannot overlap cleanup or have its active action
		// cleared by this batch. release acquires the same lock, so unlock only
		// immediately before delegating cleanup failure handling to it.
		c.actionMu.Unlock()
		if len(releaseErrors) > 0 {
			_, _ = c.release(ctx, leaseID, "input release failed")
		}
	}()
	sessionsResult, err := c.wolf.Call(ctx, "sessions", nil)
	if err != nil {
		receipt["error"] = err.Error()
		return receipt, nil
	}
	sessions, err := sanitizeList(sessionsResult["sessions"], []string{"client_id"})
	if err != nil {
		receipt["error"] = err.Error()
		return receipt, nil
	}
	if !hasClient(sessions, c.clientID) {
		receipt["error"] = "Stream session has not connected"
		return receipt, nil
	}
	for _, step := range steps {
		if isClosed(cancel) {
			break
		}
		kind := step["kind"].(string)
		switch kind {
		case "hold":
			for _, value := range step["keys"].([]any) {
				key, _ := integer(value, 1, 255, "key")
				heldKeys[key] = struct{}{}
				if err := c.send(ctx, keyPacket(key, true), deadline); err != nil {
					receipt["error"] = err.Error()
					return receipt, nil
				}
			}
		case "move":
			dx, _ := integer(step["dx"], -32768, 32767, "dx")
			dy, _ := integer(step["dy"], -32768, 32767, "dy")
			payload := make([]byte, 4)
			binary.BigEndian.PutUint16(payload, uint16(int16(dx)))
			binary.BigEndian.PutUint16(payload[2:], uint16(int16(dy)))
			if err := c.send(ctx, packet(7, payload), deadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
		case "point":
			x, _ := integer(step["x"], 0, 16383, "x")
			y, _ := integer(step["y"], 0, 16383, "y")
			width, _ := integer(step["width"], 1, 16384, "width")
			height, _ := integer(step["height"], 1, 16384, "height")
			payload := make([]byte, 10)
			for i, value := range []int{x, y, 0, width, height} {
				binary.BigEndian.PutUint16(payload[i*2:], uint16(int16(value)))
			}
			if err := c.send(ctx, packet(5, payload), deadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
		case "click":
			button, _ := integer(defaultValue(step, "button", 1), 1, 3, "button")
			heldButtons[button] = struct{}{}
			if err := c.send(ctx, packet(8, []byte{byte(button)}), deadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
		case "gamepad":
			c.mu.Lock()
			var saveErr error
			if c.lease != nil {
				previous := c.lease.Gamepad
				c.lease.Gamepad = true
				saveErr = c.saveLocked()
				if saveErr != nil {
					c.lease.Gamepad = previous
				}
			} else {
				saveErr = errors.New("lease was released before gamepad delivery")
			}
			c.mu.Unlock()
			if saveErr != nil {
				receipt["error"] = fmt.Sprintf("persist gamepad ownership: %v", saveErr)
				return receipt, nil
			}
			gamepadActive = true
			value, _ := gamepadPacket(step, true)
			if err := c.send(ctx, value, deadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
		}
		wait := time.Duration(stepMS(step)) * time.Millisecond
		if remaining := deadline.Sub(c.now()); remaining < wait {
			wait = remaining
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-cancel:
		case <-ctx.Done():
			receipt["error"] = ctx.Err().Error()
			return receipt, nil
		case <-time.After(wait):
		}
		cleanupDeadline := deadline.Add(2 * time.Second)
		for key := range heldKeys {
			if err := c.send(ctx, keyPacket(key, false), cleanupDeadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
			delete(heldKeys, key)
		}
		for button := range heldButtons {
			if err := c.send(ctx, packet(9, []byte{byte(button)}), cleanupDeadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
			delete(heldButtons, button)
		}
		if gamepadActive {
			neutral, _ := gamepadPacket(map[string]any{}, true)
			if err := c.send(ctx, neutral, cleanupDeadline); err != nil {
				receipt["error"] = err.Error()
				return receipt, nil
			}
			gamepadActive = false
		}
		receipt["completed_steps"] = receipt["completed_steps"].(int) + 1
	}
	return receipt, nil
}

func tryLock(m *sync.Mutex) bool { return m.TryLock() }
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func appendEvidenceError(receipt map[string]any, err error) {
	if previous, ok := receipt["evidence_error"].(string); ok && previous != "" {
		receipt["evidence_error"] = previous + "; " + err.Error()
		return
	}
	receipt["evidence_error"] = err.Error()
}

func (c *Controller) release(ctx context.Context, leaseID, reason string) (map[string]any, error) {
	persistenceErrors := []string{}
	c.mu.Lock()
	if c.lease == nil {
		c.mu.Unlock()
		return map[string]any{"released": true, "already_released": true}, nil
	}
	if c.lease.ID != leaseID {
		c.mu.Unlock()
		return nil, errors.New("Cannot release another lease")
	}
	c.lease.Closing = true
	if err := c.saveLocked(); err != nil {
		persistenceErrors = append(persistenceErrors, "persist closing lease: "+err.Error())
	}
	if c.cancel != nil {
		close(c.cancel)
		c.cancel = nil
	}
	c.mu.Unlock()
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	c.mu.Lock()
	if c.lease == nil {
		c.mu.Unlock()
		return map[string]any{"released": true, "already_released": true}, nil
	}
	if c.lease.ID != leaseID {
		c.mu.Unlock()
		return nil, errors.New("Cannot release another lease")
	}
	lease := *c.lease
	c.mu.Unlock()
	errorsList := persistenceErrors
	sessions, lobbies, err := c.snapshot(ctx)
	if err != nil {
		errorsList = append(errorsList, err.Error())
	} else {
		// Snapshot discovers lobbies created after acquisition. Take the copy
		// afterwards so expiry cleans those target-owned resources as well.
		c.mu.Lock()
		lease = *c.lease
		c.mu.Unlock()
		if lease.Gamepad && hasClient(sessions, c.clientID) {
			if neutral, packetErr := gamepadPacket(map[string]any{}, true); packetErr != nil {
				errorsList = append(errorsList, packetErr.Error())
			} else if err := c.send(ctx, neutral, c.now().Add(2*time.Second)); err != nil {
				errorsList = append(errorsList, "Gamepad neutral delivery: "+err.Error())
			}
		}
		for _, lobby := range lobbies {
			id := stringValue(lobby["id"])
			if contains(lease.Lobbies, id) {
				if lobbyHasOthers(lobby["connected_sessions"], c.clientID) {
					errorsList = append(errorsList, fmt.Sprintf("Lobby %s has other clients; retained", id))
				} else if _, err := c.wolf.Call(ctx, "lobbies/stop", map[string]any{"lobby_id": id}); err != nil {
					errorsList = append(errorsList, err.Error())
				}
			}
		}
		if hasClient(sessions, c.clientID) {
			latest, callErr := c.wolf.Call(ctx, "sessions", nil)
			if callErr != nil {
				errorsList = append(errorsList, callErr.Error())
			} else {
				current, listErr := sanitizeList(latest["sessions"], []string{"client_id"})
				if listErr != nil {
					errorsList = append(errorsList, listErr.Error())
				} else if hasClient(current, c.clientID) {
					if _, err := c.wolf.Call(ctx, "sessions/stop", map[string]any{"session_id": c.clientID}); err != nil {
						errorsList = append(errorsList, err.Error())
					}
				}
			}
		}
		remainingSessions, remainingLobbies, verifyErr := c.snapshot(ctx)
		if verifyErr != nil {
			errorsList = append(errorsList, verifyErr.Error())
		} else {
			if hasClient(remainingSessions, c.clientID) {
				errorsList = append(errorsList, "Session still present")
			}
			remaining := []string{}
			for _, lobby := range remainingLobbies {
				if contains(lease.Lobbies, stringValue(lobby["id"])) {
					remaining = append(remaining, stringValue(lobby["id"]))
				}
			}
			if len(remaining) > 0 {
				errorsList = append(errorsList, fmt.Sprintf("Lobbies still present: %v", remaining))
			}
		}
	}
	result := map[string]any{"event": "release", "lease_id": leaseID, "reason": reason, "released": len(errorsList) == 0, "errors": errorsList, "time": c.now().UTC().Format(time.RFC3339)}
	c.mu.Lock()
	c.lastRelease = result
	if len(errorsList) == 0 {
		c.lease = nil
	}
	if err := c.saveLocked(); err != nil {
		errorsList = append(errorsList, "persist release state: "+err.Error())
		// Do not admit another lease when the previous durable lease could not be
		// cleared. A later sweep will retry cleanup from the retained state.
		if c.lease == nil {
			retained := lease
			c.lease = &retained
		}
	}
	result["released"] = len(errorsList) == 0
	result["errors"] = errorsList
	if err := c.record(result); err != nil {
		appendEvidenceError(result, fmt.Errorf("record release receipt: %w", err))
		result["released"] = false
	}
	c.mu.Unlock()
	return result, nil
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func lobbyHasOthers(raw any, clientID string) bool {
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if stringValue(value) != clientID {
			return true
		}
	}
	return false
}

// Sweep expires a lease independently from subsequent request arrival.
func (c *Controller) Sweep(ctx context.Context) error {
	c.mu.Lock()
	if c.lease == nil {
		c.mu.Unlock()
		return nil
	}
	expired := c.lease.ExpiresAt <= float64(c.now().UnixNano())/1e9 || c.lease.Closing
	leaseID := c.lease.ID
	c.mu.Unlock()
	if !expired {
		_, _, err := c.snapshot(ctx)
		return err
	}
	_, err := c.release(ctx, leaseID, "expired or recovering")
	return err
}

func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	id := ""
	if c.lease != nil {
		id = c.lease.ID
	}
	c.mu.Unlock()
	if id == "" {
		return nil
	}
	_, err := c.release(ctx, id, "service shutdown")
	return err
}

func resolveInputMounts(wolfStateDir, clientID string) ([]string, error) {
	dataDir, err := uiUdevDataDir(wolfStateDir, clientID)
	if err != nil {
		return nil, err
	}
	minor, err := waitForFreshJoystickMinor(dataDir)
	if err != nil {
		return nil, err
	}
	return inputDeviceMounts("/dev/input", "/sys/class/input", minor)
}

func clearStaleJoystickState(wolfStateDir, clientID string) error {
	dataDir, err := uiUdevDataDir(wolfStateDir, clientID)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Wolf UI input state: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || inputHWDBName.MatchString(entry.Name()) == false {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read Wolf UI input state %s: %w", entry.Name(), err)
		}
		if bytes.Contains(contents, []byte("E:ID_INPUT_JOYSTICK=1")) {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove stale Wolf UI gamepad state %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func uiUdevDataDir(wolfStateDir, clientID string) (string, error) {
	if clientID == "" {
		return "", errors.New("Moonlight client ID is empty")
	}
	if _, err := strconv.ParseUint(clientID, 10, 64); err != nil {
		return "", errors.New("Moonlight client ID is not a numeric Wolf session ID")
	}
	return filepath.Join(wolfStateDir, clientID, "Wolf UI", "udev", "data"), nil
}

func waitForFreshJoystickMinor(dataDir string) (uint32, error) {
	deadline := time.Now().Add(inputReadyTimeout)
	for {
		minors, err := joystickMinors(dataDir)
		if err == nil && len(minors) == 1 {
			return minors[0], nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if err == nil && len(minors) > 1 {
			return 0, fmt.Errorf("expected exactly one fresh virtual gamepad in %s, found %d", dataDir, len(minors))
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("expected exactly one fresh virtual gamepad in %s, found 0 after %s", dataDir, inputReadyTimeout)
		}
		time.Sleep(inputReadyPoll)
	}
}

func joystickMinors(dataDir string) ([]uint32, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("read Wolf UI input state: %w", err)
	}
	minors := []uint32{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := inputHWDBName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(dataDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read Wolf UI input state %s: %w", entry.Name(), err)
		}
		if !bytes.Contains(contents, []byte("E:ID_INPUT_JOYSTICK=1")) {
			continue
		}
		minor, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse virtual gamepad minor: %w", err)
		}
		minors = append(minors, uint32(minor))
	}
	sort.Slice(minors, func(i, j int) bool { return minors[i] < minors[j] })
	return minors, nil
}

func inputDeviceMounts(inputDir, sysInputDir string, minor uint32) ([]string, error) {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return nil, fmt.Errorf("read input devices: %w", err)
	}
	var eventPath string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "event") {
			continue
		}
		path := filepath.Join(inputDir, entry.Name())
		var stat syscall.Stat_t
		if err := syscall.Stat(path, &stat); err != nil || stat.Rdev == 0 {
			continue
		}
		if linuxDeviceMajor(uint64(stat.Rdev)) == 13 && linuxDeviceMinor(uint64(stat.Rdev)) == minor {
			eventPath = path
			break
		}
	}
	if eventPath == "" {
		return nil, fmt.Errorf("Wolf virtual gamepad c13:%d has no event device", minor)
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(sysInputDir, filepath.Base(eventPath), "device"))
	if err != nil {
		return nil, fmt.Errorf("resolve virtual gamepad sysfs device: %w", err)
	}
	paths := []string{eventPath}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "js") {
			continue
		}
		path := filepath.Join(inputDir, entry.Name())
		candidate, err := filepath.EvalSymlinks(filepath.Join(sysInputDir, entry.Name(), "device"))
		if err == nil && candidate == parent {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func linuxDeviceMajor(device uint64) uint32 {
	return uint32((device>>8)&0xfff | (device>>32)&0xfffff000)
}
func linuxDeviceMinor(device uint64) uint32 { return uint32(device&0xff | (device>>12)&0xffffff00) }

func setIsolatedInputMounts(runner map[string]any, devicePaths []string) error {
	values, err := asSlice(defaultValue(runner, "mounts", []any{}), "runner mounts")
	if err != nil {
		return err
	}
	mounts := make([]any, 0, len(values)+len(devicePaths))
	for _, value := range values {
		mount, ok := value.(string)
		if !ok {
			return errors.New("runner mounts must contain strings")
		}
		parts := strings.Split(mount, ":")
		if len(parts) >= 2 && parts[1] == "/dev/input" {
			continue
		}
		mounts = append(mounts, mount)
	}
	for _, path := range devicePaths {
		if !strings.HasPrefix(path, "/dev/input/") || strings.Contains(path, ":") {
			return fmt.Errorf("unsafe isolated input path %q", path)
		}
		mounts = append(mounts, path+":"+path+":ro")
	}
	runner["mounts"] = mounts
	return nil
}

func DefaultPort() int { return defaultPort }

// Only the installed Wolf profile can opt into the managed browser entrypoint.
// The URL is data passed as argv/environment, never a shell command.
func configureBrowserRunner(runner map[string]any, gameURL string) (string, string, error) {
	compositor, startup := "sway", "native-navigation"
	values, err := asSlice(defaultValue(runner, "env", []any{}), "runner env")
	if err != nil {
		return "", "", err
	}
	env := make([]any, 0, len(values)+3)
	for _, value := range values {
		entry, ok := value.(string)
		if !ok {
			return "", "", errors.New("runner env must contain strings")
		}
		if entry == "RUN_GAMESCOPE=1" {
			compositor = "gamescope"
		}
		if entry == "PLAYTEST_BROWSER_STARTUP=1" {
			startup = "kiosk"
		}
		if !strings.HasPrefix(entry, "PLAYTEST_HOST=") && !strings.HasPrefix(entry, "PLAYTEST_PORT=") && !strings.HasPrefix(entry, "PLAYTEST_URL=") && !strings.HasPrefix(entry, "MOZ_LEGACY_PROFILES=") {
			env = append(env, entry)
		}
	}
	// New Firefox profile migration otherwise creates an unprepared
	// default-release profile before it reads the dedicated user.js.
	env = append(env, "MOZ_LEGACY_PROFILES=1")
	if startup == "kiosk" {
		u, err := url.Parse(gameURL)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil {
			return "", "", errors.New("managed browser requires an HTTP game URL without credentials")
		}
		port := u.Port()
		if port == "" {
			port = "80"
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", "", errors.New("invalid game URL port")
		}
		host := u.Hostname()
		u.Host = "localhost:" + port
		env = append(env, "PLAYTEST_HOST="+host, "PLAYTEST_PORT="+port, "PLAYTEST_URL="+u.String())
	}
	runner["env"] = env
	return compositor, startup, nil
}
