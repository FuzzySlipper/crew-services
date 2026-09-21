// Package browser provides a private Playwright-backed browser lease for the
// shared playtest session service.
package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"crew-services/internal/playtest/evidence"
	"crew-services/internal/playtest/session"
	"github.com/google/uuid"
)

const (
	operationTimeout = 15 * time.Second
	shutdownTimeout  = 5 * time.Second
	maxRPCBytes      = 24 << 20 // includes base64 encoding of a 16 MiB PNG
	maxScreenshot    = 16 << 20
)

func capabilities() map[string]any {
	return map[string]any{
		"browser_inspect":       true,
		"dom_actions":           true,
		"absolute_pointer":      true,
		"keyboard":              true,
		"gamepad":               true,
		"relative_mouse":        false,
		"pointer_lock_readback": true,
		"engine_queries":        false,
	}
}

// Config names only local executables and private state owned by this adapter.
type Config struct {
	State    string
	Node     string
	Worker   string
	Chromium string
}

// Adapter owns one private Chromium child and persistent profile at a time.
// A killed child deliberately makes its lease unavailable rather than replaying
// the operation against a replacement browser.
type Adapter struct {
	config Config

	mu          sync.Mutex
	journalMu   sync.Mutex
	profile     session.Profile
	leaseID     string
	directory   string
	child       *child
	active      int
	unavailable string
	closed      bool
}

type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu        sync.Mutex
	nextID    int
	pending   map[int]chan rpcResponse
	stopped   bool
	onFailure func(error)
}

type rpcResponse struct {
	result map[string]any
	err    error
}

type rpcMessage struct {
	ID     int            `json:"id"`
	Result map[string]any `json:"result"`
	Error  string         `json:"error"`
}

// New validates fixed executable paths and creates the private browser state
// directory. It does not launch Chromium until Acquire.
func New(config Config) (*Adapter, error) {
	state, err := absoluteDirectory(config.State, "browser state")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("create browser state directory: %w", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, fmt.Errorf("protect browser state directory: %w", err)
	}
	config.State = state
	if config.Node == "" {
		config.Node = "node"
	}
	node, err := exec.LookPath(config.Node)
	if err != nil {
		return nil, fmt.Errorf("find Node executable: %w", err)
	}
	config.Node = node
	worker, err := absoluteFile(config.Worker, "browser worker")
	if err != nil {
		return nil, err
	}
	config.Worker = worker
	if config.Chromium != "" {
		chromium, err := absoluteFile(config.Chromium, "Chromium executable")
		if err != nil {
			return nil, err
		}
		config.Chromium = chromium
	}
	return &Adapter{config: config}, nil
}

func absoluteDirectory(value, label string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return path, nil
}

func absoluteFile(value, label string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s must be a file", label)
	}
	return path, nil
}

// SelectProfile admits only browser/service profiles before a lease exists.
func (a *Adapter) SelectProfile(profile session.Profile) error {
	if profile.Backend != "" && profile.Backend != "browser" {
		return fmt.Errorf("capability_unavailable: browser adapter cannot run backend %q", profile.Backend)
	}
	if profile.Environment != "" && profile.Environment != "service" {
		return fmt.Errorf("capability_unavailable: browser adapter requires service environment, got %q", profile.Environment)
	}
	if strings.TrimSpace(profile.URL) == "" {
		return errors.New("browser profile URL is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID != "" {
		return errors.New("release the existing browser lease before selecting another profile")
	}
	a.profile = profile
	return nil
}

// Acquire starts one fixed Node Playwright driver and a headless persistent
// context. The selected profile is navigated only by Launch.
func (a *Adapter) Acquire(ctx context.Context, width, height, fps, ttl int) (map[string]any, error) {
	if width < 320 || width > 3840 || height < 240 || height > 2160 || fps < 1 || fps > 120 || ttl < 1 {
		return nil, errors.New("browser acquisition received invalid viewport, FPS, or TTL")
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("browser adapter is closed")
	}
	if a.leaseID != "" {
		a.mu.Unlock()
		return nil, errors.New("release the existing browser lease before acquiring another")
	}
	if a.profile.ID == "" {
		a.mu.Unlock()
		return nil, errors.New("select a browser profile before acquiring")
	}
	leaseID := uuid.NewString()
	directory := filepath.Join(a.config.State, leaseID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("create browser lease directory: %w", err)
	}
	child, err := startChild(a.config, directory)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.leaseID, a.directory, a.child, a.unavailable = leaseID, directory, child, ""
	child.mu.Lock()
	child.onFailure = func(cause error) { a.abandon(child, "browser driver failure: "+cause.Error()) }
	stopped := child.stopped
	child.mu.Unlock()
	a.mu.Unlock()
	if stopped {
		a.abandon(child, "browser driver exited before launch")
		return nil, errors.New("browser driver exited before launch")
	}

	result, err := a.call(ctx, child, "launch", map[string]any{
		"width": width, "height": height, "headless": true,
		"user_data_dir": filepath.Join(directory, "profile"), "executable_path": a.config.Chromium,
	})
	if err != nil {
		a.abandon(child, "browser launch failed")
		a.mu.Lock()
		if a.child == child {
			a.leaseID, a.directory, a.child, a.active, a.unavailable = "", "", nil, 0, ""
		}
		a.mu.Unlock()
		return nil, fmt.Errorf("browser launch: %w", err)
	}
	result["lease_id"] = leaseID
	result["artifact_directory"] = directory
	result["events_path"] = filepath.Join(directory, "events.jsonl")
	result["game_readiness"] = "unknown"
	result["capabilities"] = capabilities()
	return result, nil
}

// Launch navigates the already-owned persistent page to the selected profile.
func (a *Adapter) Launch(ctx context.Context, leaseID string, profile session.Profile) (map[string]any, error) {
	if err := a.SelectLaunchProfile(leaseID, profile); err != nil {
		return nil, err
	}
	child := a.currentChild(leaseID)
	if child == nil {
		return nil, a.unavailableError(leaseID)
	}
	result, err := a.call(ctx, child, "navigate", map[string]any{"url": profile.URL})
	if err != nil {
		return nil, fmt.Errorf("browser navigate: %w", err)
	}
	result["server_http_ready"] = true
	a.mu.Lock()
	result["events_path"] = filepath.Join(a.directory, "events.jsonl")
	a.mu.Unlock()
	result["browser_url"] = result["url"]
	result["game_readiness"] = "unknown; inspect the original screenshot"
	result["capabilities"] = capabilities()
	return result, nil
}

// SelectLaunchProfile verifies that service did not switch profile identity
// between selection and launch.
func (a *Adapter) SelectLaunchProfile(leaseID string, profile session.Profile) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID != leaseID {
		return errors.New("this adapter does not own that browser lease")
	}
	if a.unavailable != "" {
		return fmt.Errorf("browser lease unavailable: %s; stop or recover explicitly", a.unavailable)
	}
	if profile.ID != a.profile.ID || profile.URL != a.profile.URL {
		return errors.New("browser profile changed after acquisition")
	}
	return nil
}

// Observe writes an immutable original PNG and a JSON sidecar in the lease
// directory. Renderer stays unknown unless the product itself reports it.
func (a *Adapter) Observe(ctx context.Context, leaseID string) (map[string]any, error) {
	child := a.currentChild(leaseID)
	if child == nil {
		return nil, a.unavailableError(leaseID)
	}
	started := time.Now().UTC()
	result, err := a.call(ctx, child, "observe", nil)
	ended := time.Now().UTC()
	if err != nil {
		return nil, fmt.Errorf("browser observe: %w", err)
	}
	encoded, _ := result["screenshot_base64"].(string)
	if encoded == "" || len(encoded) > maxScreenshot*2 {
		return nil, errors.New("browser observation omitted a bounded screenshot")
	}
	pixels, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(pixels) == 0 || len(pixels) > maxScreenshot {
		return nil, errors.New("browser observation returned an invalid screenshot")
	}
	decoded, _, err := image.DecodeConfig(bytes.NewReader(pixels))
	if err != nil || decoded.Width < 1 || decoded.Height < 1 {
		return nil, errors.New("browser observation returned a screenshot without valid PNG dimensions")
	}
	delete(result, "screenshot_base64")
	a.mu.Lock()
	directory := a.directory
	a.mu.Unlock()
	artifactID := uuid.NewString()
	path := filepath.Join(directory, artifactID+".png")
	if err := writeFile(path, pixels); err != nil {
		return nil, fmt.Errorf("persist browser screenshot: %w", err)
	}
	result["event"] = "observation"
	result["lease_id"] = leaseID
	result["artifact_id"] = artifactID
	result["path"] = path
	result["events_path"] = filepath.Join(directory, "events.jsonl")
	result["renderer"] = "unknown"
	result["width"] = decoded.Width
	result["height"] = decoded.Height
	result["capture_started_at"] = started
	result["capture_ended_at"] = ended
	result["capture_started_at_ns"] = started.UnixNano()
	result["capture_ended_at_ns"] = ended.UnixNano()
	result["source"] = "headless Chromium screenshot through the private Playwright context"
	result["game_readiness"] = "unknown"
	result["frame_freshness"] = "not measured"
	metadataPath := filepath.Join(directory, artifactID+".json")
	result["metadata_path"] = metadataPath
	if err := evidence.WriteJSONFile(metadataPath, result); err != nil {
		return result, fmt.Errorf("persist browser observation metadata: %w", err)
	}
	return result, nil
}

func writeFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".screenshot-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	for len(data) > 0 {
		written, err := temporary.Write(data)
		if written < 0 || written > len(data) {
			_ = temporary.Close()
			return errors.New("screenshot writer reported an invalid byte count")
		}
		data = data[written:]
		if err != nil {
			_ = temporary.Close()
			return err
		}
		if written == 0 {
			_ = temporary.Close()
			return io.ErrShortWrite
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return evidence.SyncDirectory(filepath.Dir(path))
}

// Input delivers browser-native key/mouse steps and finite standard-Gamepad
// snapshots. The child injects only its virtual device through the page's
// Gamepad API; it never reads or changes product state.
func (a *Adapter) Input(ctx context.Context, leaseID string, steps []map[string]any) (map[string]any, error) {
	child := a.currentChild(leaseID)
	if child == nil {
		return nil, a.unavailableError(leaseID)
	}
	return a.call(ctx, child, "input", map[string]any{"steps": steps})
}

// Browser forwards a bounded DOM operation to the private Playwright page.
func (a *Adapter) Browser(ctx context.Context, leaseID string, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 64*1024 || !json.Valid(raw) {
		return nil, errors.New("browser data must be a JSON object up to 65536 bytes")
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil || request == nil {
		return nil, errors.New("browser data must be a JSON object")
	}
	child := a.currentChild(leaseID)
	if child == nil {
		return nil, a.unavailableError(leaseID)
	}
	return a.call(ctx, child, "browser", request)
}

// Cancel leaves an idle persistent browser alive. Cancellation during an RPC
// kills and waits for the process group, so its action cannot continue after
// the caller returns; recovery is deliberately explicit.
func (a *Adapter) Cancel(_ context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if a.leaseID != leaseID {
		a.mu.Unlock()
		return nil, errors.New("this adapter does not own that browser lease")
	}
	child, active, unavailable := a.child, a.active, a.unavailable
	a.mu.Unlock()
	if unavailable != "" {
		return map[string]any{"cancelled": false, "lease_state": "unavailable", "requires_recover": true}, nil
	}
	if active == 0 || child == nil {
		return map[string]any{"cancelled": false, "lease_state": "active", "browser_retained": true}, nil
	}
	a.abandon(child, "cancelled while browser operation was active")
	return map[string]any{"cancelled": true, "lease_state": "unavailable", "requires_recover": true}, nil
}

// Status returns the currently owned lease when the service performs its
// read-only global status query with an empty lease ID.
func (a *Adapter) Status(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if leaseID == "" {
		leaseID = a.leaseID
	}
	if leaseID == "" {
		a.mu.Unlock()
		return map[string]any{}, nil
	}
	child, unavailable := a.child, a.unavailable
	owned := a.leaseID == leaseID
	a.mu.Unlock()
	if !owned {
		return nil, errors.New("this adapter does not own that browser lease")
	}
	if unavailable != "" || child == nil {
		return map[string]any{"lease": map[string]any{"id": leaseID, "state": "unavailable", "reason": unavailable}}, nil
	}
	result, err := a.call(ctx, child, "status", nil)
	if err != nil {
		return nil, err
	}
	result["lease"] = map[string]any{"id": leaseID, "state": "active"}
	result["capabilities"] = capabilities()
	return result, nil
}

// Release closes the child, awaits its exit, and forgets the local lease.
func (a *Adapter) Release(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if a.leaseID == "" {
		a.mu.Unlock()
		return map[string]any{"released": true, "local_browser": "not_owned_after_recovery"}, nil
	}
	if a.leaseID != leaseID {
		a.mu.Unlock()
		return nil, errors.New("this adapter does not own that browser lease")
	}
	child, directory := a.child, a.directory
	a.mu.Unlock()
	if child != nil {
		_, _ = a.call(ctx, child, "release", nil)
		stopChild(child)
	}
	receipt := map[string]any{"released": true, "browser_closed": child != nil, "artifact_directory": directory}
	journalErr := a.journal(directory, map[string]any{"event": "release", "lease_id": leaseID, "receipt": receipt})
	if journalErr != nil {
		receipt["evidence_error"] = journalErr.Error()
	}
	a.mu.Lock()
	if a.leaseID == leaseID {
		a.leaseID, a.directory, a.child, a.active, a.unavailable = "", "", nil, 0, ""
	}
	a.mu.Unlock()
	return receipt, journalErr
}

// Close releases the local child without attempting to adopt a saved lease.
func (a *Adapter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	leaseID := a.leaseID
	a.mu.Unlock()
	if leaseID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_, err := a.Release(ctx, leaseID)
	return err
}

func (a *Adapter) currentChild(leaseID string) *child {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID != leaseID || a.unavailable != "" || a.child == nil {
		return nil
	}
	return a.child
}

func (a *Adapter) unavailableError(leaseID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID != leaseID {
		return errors.New("this adapter does not own that browser lease")
	}
	if a.unavailable != "" {
		return fmt.Errorf("browser lease unavailable: %s; stop or recover explicitly", a.unavailable)
	}
	return errors.New("browser lease is unavailable")
}

func (a *Adapter) call(ctx context.Context, child *child, method string, params map[string]any) (map[string]any, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, operationTimeout)
		defer cancel()
	}
	a.mu.Lock()
	if a.child != child || a.unavailable != "" {
		leaseID := a.leaseID
		a.mu.Unlock()
		return nil, a.unavailableError(leaseID)
	}
	a.active++
	directory, leaseID := a.directory, a.leaseID
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.child == child && a.active > 0 {
			a.active--
		}
		a.mu.Unlock()
	}()
	// Read-only status and large screenshot payloads have their own observations.
	// Action records cover manual CLI/MCP calls as well as supervised scripts.
	logged := method == "browser" || method == "input" || method == "navigate"
	operationID := uuid.NewString()
	if logged {
		if err := a.journal(directory, map[string]any{"event": "action_requested", "operation_id": operationID, "lease_id": leaseID, "method": method, "params": params}); err != nil {
			return nil, fmt.Errorf("persist browser action request before delivery: %w", err)
		}
	}
	response, err := child.call(ctx, method, params)
	if err != nil && ctx.Err() != nil {
		a.abandon(child, "browser operation context cancelled or timed out")
	}
	if logged {
		record := map[string]any{"event": "action_result", "operation_id": operationID, "lease_id": leaseID, "result": response}
		if err != nil {
			record["error"] = err.Error()
		}
		if journalErr := a.journal(directory, record); journalErr != nil {
			if response == nil {
				response = map[string]any{}
			}
			response["evidence_error"] = journalErr.Error()
			err = errors.Join(err, fmt.Errorf("persist browser action result; do not replay automatically: %w", journalErr))
		}
	}
	return response, err
}

func (a *Adapter) journal(directory string, value map[string]any) error {
	a.journalMu.Lock()
	defer a.journalMu.Unlock()
	value["recorded_at"] = time.Now().UTC()
	return evidence.AppendJSONFile(filepath.Join(directory, "events.jsonl"), value)
}

func (a *Adapter) abandon(child *child, reason string) {
	a.mu.Lock()
	if a.child == child {
		a.unavailable = reason
		a.active = 0
	}
	a.mu.Unlock()
	stopChild(child)
}

func startChild(config Config, directory string) (*child, error) {
	log, err := os.OpenFile(filepath.Join(directory, "browser-driver.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open browser driver log: %w", err)
	}
	command := exec.Command(config.Node, config.Worker)
	command.Env = scrubbedEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = log.Close()
		return nil, err
	}
	command.Stderr = log
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = log.Close()
		return nil, fmt.Errorf("start browser driver: %w", err)
	}
	c := &child{cmd: command, stdin: stdin, done: make(chan struct{}), pending: map[int]chan rpcResponse{}}
	go c.read(stdout)
	go func() {
		_ = command.Wait()
		_ = log.Close()
		c.failPending(errors.New("browser driver exited"))
		c.mu.Lock()
		stopped, callback := c.stopped, c.onFailure
		c.mu.Unlock()
		if !stopped && callback != nil {
			go callback(errors.New("browser driver exited"))
		}
		close(c.done)
	}()
	return c, nil
}

func scrubbedEnvironment() []string {
	value := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "PASSWORD") {
			continue
		}
		value = append(value, entry)
	}
	return value
}

func (c *child) read(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxRPCBytes)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || message.ID < 1 || (message.Result == nil && message.Error == "") {
			failure := errors.New("invalid browser driver response")
			c.failPending(failure)
			c.reportFailure(failure)
			return
		}
		c.mu.Lock()
		response := c.pending[message.ID]
		delete(c.pending, message.ID)
		c.mu.Unlock()
		if response == nil {
			continue
		}
		if message.Error != "" {
			response <- rpcResponse{err: errors.New(message.Error)}
		} else {
			response <- rpcResponse{result: message.Result}
		}
	}
	if err := scanner.Err(); err != nil {
		failure := fmt.Errorf("read browser driver: %w", err)
		c.failPending(failure)
		c.reportFailure(failure)
		return
	}
	failure := io.ErrUnexpectedEOF
	c.failPending(failure)
	c.reportFailure(failure)
}

func (c *child) reportFailure(err error) {
	c.mu.Lock()
	stopped, callback := c.stopped, c.onFailure
	c.mu.Unlock()
	if !stopped && callback != nil {
		go callback(err)
	}
}

func (c *child) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil, errors.New("browser driver is stopped")
	}
	c.nextID++
	id := c.nextID
	response := make(chan rpcResponse, 1)
	c.pending[id] = response
	message, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err == nil {
		_, err = c.stdin.Write(append(message, '\n'))
	}
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write browser driver request: %w", err)
	}
	c.mu.Unlock()
	select {
	case result := <-response:
		return result.result, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("browser driver exited")
	}
}

func (c *child) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, response := range c.pending {
		response <- rpcResponse{err: err}
		delete(c.pending, id)
	}
}

func stopChild(c *child) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		<-c.done
		return
	}
	c.stopped = true
	pid := 0
	if c.cmd.Process != nil {
		pid = c.cmd.Process.Pid
	}
	c.mu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	select {
	case <-c.done:
	case <-time.After(shutdownTimeout):
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		<-c.done
	}
}

var _ session.Backend = (*Adapter)(nil)
var _ session.ProfileBackend = (*Adapter)(nil)
var _ session.Launcher = (*Adapter)(nil)
var _ session.BrowserBackend = (*Adapter)(nil)
