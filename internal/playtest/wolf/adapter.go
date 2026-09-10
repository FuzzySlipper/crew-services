// Package wolf captures a Wolf stream locally while the remote controller owns
// lease expiry, input delivery, and native session cleanup.
package wolf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"crew-services/internal/playtest/evidence"
	"github.com/google/uuid"
)

const (
	remoteTimeout      = 30 * time.Second
	cancelTimeout      = 5 * time.Second
	processStopTimeout = 5 * time.Second
	streamStartTimeout = 45 * time.Second
	xvfbStartTimeout   = 10 * time.Second
	streamPollInterval = 300 * time.Millisecond
	watchInterval      = 2 * time.Second
	defaultBitrate     = 6000
	defaultApplication = "Wolf UI"
)

var moonlightWindow = regexp.MustCompile(`(?m)^\s*(0x[0-9a-f]+) ".* - Moonlight":`)

// ErrCleanupUncertain means target input cancellation reported one or more
// failures while releasing held controls.
var ErrCleanupUncertain = errors.New("Wolf input cleanup is uncertain")

// Config names the local capture programs and the existing remote target
// service. Its JSON fields match the established Python bridge configuration.
type Config struct {
	SSHHost         string `json:"ssh_host"`
	TargetPort      int    `json:"target_port"`
	StreamHost      string `json:"stream_host"`
	Moonlight       string `json:"moonlight"`
	MoonlightConfig string `json:"moonlight_config"`
	Application     string `json:"application"`
	State           string `json:"state"`
	Bitrate         int    `json:"bitrate"`
}

// Adapter owns one local Moonlight capture slot. The remote target owns the
// Wolf stream session and always receives the input action ID before delivery.
type Adapter struct {
	config Config
	// remoteCall is used only by focused package tests; production uses remote.
	remoteCall func(context.Context, string, map[string]any) (map[string]any, error)

	mu           sync.Mutex
	journalMu    sync.Mutex
	lockFile     *os.File
	lease        map[string]any
	directory    string
	env          []string
	window       string
	xvfb         *managedProcess
	moonlight    *managedProcess
	activeAction string
	closing      bool
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	stopWatch    chan struct{}
	watchDone    chan struct{}
}

type managedProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	log    *os.File
	done   chan struct{}
}

type remoteEnvelope struct {
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result"`
	Error  string         `json:"error"`
}

// New validates capture configuration, creates the private artifact root, and
// takes the single-process lock. It does not contact the remote controller.
func New(config Config) (*Adapter, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	state, err := filepath.Abs(config.State)
	if err != nil {
		return nil, fmt.Errorf("resolve Wolf state directory: %w", err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("create Wolf state directory: %w", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, fmt.Errorf("protect Wolf state directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(state, "controller.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Wolf controller lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("Wolf capture slot is already owned: %w", err)
	}
	config.State = state
	adapter := &Adapter{
		config:    config,
		lockFile:  lockFile,
		stopWatch: make(chan struct{}),
		watchDone: make(chan struct{}),
	}
	go adapter.watch()
	return adapter, nil
}

func (config *Config) validate() error {
	config.SSHHost = strings.TrimSpace(config.SSHHost)
	config.StreamHost = strings.TrimSpace(config.StreamHost)
	config.Moonlight = strings.TrimSpace(config.Moonlight)
	config.MoonlightConfig = strings.TrimSpace(config.MoonlightConfig)
	config.Application = strings.TrimSpace(config.Application)
	config.State = strings.TrimSpace(config.State)
	if config.SSHHost == "" {
		return errors.New("Wolf SSH host is required")
	}
	if config.TargetPort < 1 || config.TargetPort > 65535 {
		return errors.New("Wolf target port must be from 1 through 65535")
	}
	if config.StreamHost == "" {
		return errors.New("Wolf stream host is required")
	}
	if config.Moonlight == "" {
		return errors.New("Moonlight executable is required")
	}
	if config.MoonlightConfig == "" {
		return errors.New("Moonlight configuration directory is required")
	}
	if config.State == "" {
		return errors.New("Wolf state directory is required")
	}
	if config.Application == "" {
		config.Application = defaultApplication
	}
	if config.Bitrate == 0 {
		config.Bitrate = defaultBitrate
	}
	if config.Bitrate < 1 {
		return errors.New("Wolf bitrate must be positive")
	}
	return nil
}

// Acquire reserves the remote controller, then starts a private Xvfb and
// Moonlight capture for the configured application, defaulting to Wolf UI.
func (a *Adapter) Acquire(ctx context.Context, width, height, fps, ttl int) (map[string]any, error) {
	if err := validateAcquire(width, height, fps, ttl); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("Wolf adapter is closed")
	}
	if a.lease != nil {
		return nil, errors.New("release the existing Wolf lease before acquiring another")
	}
	lease, err := a.remote(ctx, "acquire", map[string]any{"ttl_seconds": ttl})
	if err != nil {
		return nil, err
	}
	leaseID, clientID, err := leaseIdentity(lease)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(a.config.State, leaseID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
		defer cancel()
		_, _ = a.remote(cleanupCtx, "release", map[string]any{"lease_id": leaseID})
		return nil, fmt.Errorf("create Wolf artifact directory: %w", err)
	}
	a.lease, a.directory = lease, directory
	if err := a.journal(map[string]any{
		"event": "acquire", "lease": lease,
		"requested": map[string]any{"app": a.config.Application, "width": width, "height": height, "fps": fps},
	}); err != nil {
		return nil, a.acquireFailedLocked(leaseID, err)
	}
	if err := a.startXvfbLocked(width, height); err != nil {
		return nil, a.acquireFailedLocked(leaseID, err)
	}
	if err := a.startMoonlightLocked(width, height, fps); err != nil {
		return nil, a.acquireFailedLocked(leaseID, err)
	}

	deadline := time.Now().Add(streamStartTimeout)
	for time.Now().Before(deadline) {
		if processExited(a.moonlight) {
			return nil, a.acquireFailedLocked(leaseID, errors.New("Moonlight exited; inspect private moonlight.log"))
		}
		pollCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		status, statusErr := a.remote(pollCtx, "status", map[string]any{"lease_id": leaseID})
		cancel()
		if statusErr == nil {
			if session, found := sessionForClient(status, clientID); found {
				if window, found := a.findMoonlightWindowLocked(ctx); found {
					a.window = window
					if err := a.positionWindowLocked(ctx, window, width, height); err != nil {
						return nil, a.acquireFailedLocked(leaseID, err)
					}
					time.Sleep(time.Second)
					result := map[string]any{
						"lease_id":                leaseID,
						"session":                 session,
						"capture":                 "moonlight-x11",
						"stream_window_available": true,
						"game_readiness":          "unknown",
						"pointer_lock":            PointerLockDiagnostic(),
						"artifact_directory":      directory,
						"expires_at":              nestedValue(status, "lease", "expires_at"),
					}
					if err := a.journal(mergeEvent("stream_connected", result)); err != nil {
						return nil, a.acquireFailedLocked(leaseID, err)
					}
					return result, nil
				}
			}
		}
		if err := waitContext(ctx, streamPollInterval); err != nil {
			return nil, a.acquireFailedLocked(leaseID, err)
		}
	}
	return nil, a.acquireFailedLocked(leaseID, errors.New("no Wolf stream window/session within 45 seconds"))
}

func validateAcquire(width, height, fps, ttl int) error {
	if width < 640 || width > 3840 || height < 360 || height > 2160 {
		return errors.New("resolution must be within 640x360 through 3840x2160")
	}
	if fps != 30 && fps != 60 {
		return errors.New("Wolf capture FPS must be 30 or 60")
	}
	if ttl < 15 || ttl > 1800 {
		return errors.New("Wolf lease TTL must be from 15 through 1800 seconds")
	}
	return nil
}

func (a *Adapter) acquireFailedLocked(leaseID string, cause error) error {
	a.stopCaptureLocked()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	if _, err := a.remote(cleanupCtx, "release", map[string]any{"lease_id": leaseID}); err != nil {
		_ = a.journal(map[string]any{"event": "acquire_failed", "error": cause.Error(), "cleanup_error": err.Error()})
	} else {
		_ = a.journal(map[string]any{"event": "acquire_failed", "error": cause.Error()})
	}
	a.lease, a.directory, a.window, a.env = nil, "", "", nil
	return cause
}

// Observe writes an immutable PNG artifact in the lease directory and returns
// its path with capture metadata. It renews the remote lease through status.
func (a *Adapter) Observe(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.requireLocked(leaseID); err != nil {
		return nil, err
	}
	if processExited(a.moonlight) {
		return nil, errors.New("Moonlight has exited; release the Wolf lease")
	}
	before, err := a.remote(ctx, "status", map[string]any{"lease_id": leaseID})
	if err != nil {
		return nil, err
	}
	_, clientID, _ := leaseIdentity(a.lease)
	if _, found := sessionForClient(before, clientID); !found {
		return nil, errors.New("remote Wolf stream no longer exists")
	}
	window, found := a.findMoonlightWindowLocked(ctx)
	if !found {
		return nil, errors.New("Moonlight stream window is not viewable")
	}
	if window != a.window {
		a.window = window
		geometry, err := a.runTool(ctx, "xdotool", "getdisplaygeometry")
		if err != nil {
			return nil, err
		}
		parts := strings.Fields(geometry)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected X display geometry %q", geometry)
		}
		if _, err := a.runTool(ctx, "xdotool", "windowsize", window, parts[0], parts[1], "windowmove", window, "0", "0"); err != nil {
			return nil, err
		}
	}
	artifactID := uuid.NewString()
	path := filepath.Join(a.directory, artifactID+".png")
	temporary := filepath.Join(a.directory, artifactID+".tmp.png")
	started := time.Now().UnixNano()
	if _, err := a.runTool(ctx, "import", "-window", window, temporary); err != nil {
		return nil, err
	}
	ended := time.Now().UnixNano()
	file, err := os.Open(temporary)
	if err != nil {
		return nil, fmt.Errorf("open captured Wolf artifact: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return nil, fmt.Errorf("sync captured Wolf artifact: %w", syncErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close captured Wolf artifact: %w", closeErr)
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, fmt.Errorf("publish captured Wolf artifact: %w", err)
	}
	if err := evidence.SyncDirectory(a.directory); err != nil {
		return nil, fmt.Errorf("sync captured Wolf artifact directory: %w", err)
	}
	dimensions, err := a.runTool(ctx, "identify", "-format", "%w %h", path)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(dimensions)
	if len(fields) != 2 {
		return nil, fmt.Errorf("unexpected capture dimensions %q", dimensions)
	}
	width, widthErr := strconv.Atoi(fields[0])
	height, heightErr := strconv.Atoi(fields[1])
	if widthErr != nil || heightErr != nil {
		return nil, fmt.Errorf("parse capture dimensions %q", dimensions)
	}
	metadata := map[string]any{
		"event":                 "observation",
		"lease_id":              leaseID,
		"artifact_id":           artifactID,
		"path":                  path,
		"capture_started_at_ns": started,
		"capture_ended_at_ns":   ended,
		"width":                 width,
		"height":                height,
		"source":                "remote Wolf stream decoded by Moonlight into private X11 window",
		"game_frame_id":         nil,
		"game_readiness":        "unknown",
		"frame_freshness":       "not measured",
		"target":                before,
	}
	if err := a.journal(metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

// Input sends one ordered target batch. If ctx is cancelled, it sends cancel
// with the already assigned action ID and waits for the target batch cleanup.
func (a *Adapter) Input(ctx context.Context, leaseID string, steps []map[string]any) (map[string]any, error) {
	a.mu.Lock()
	if err := a.requireLocked(leaseID); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if a.activeAction != "" {
		a.mu.Unlock()
		return nil, errors.New("Wolf input batch is already running")
	}
	actionID := uuid.NewString()
	a.activeAction = actionID
	directory := a.directory
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.activeAction == actionID {
			a.activeAction = ""
		}
		a.mu.Unlock()
	}()

	targetCtx, targetCancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer targetCancel()
	type inputResult struct {
		value map[string]any
		err   error
	}
	done := make(chan inputResult, 1)
	go func() {
		value, err := a.remote(targetCtx, "input", map[string]any{
			"lease_id": leaseID, "action_id": actionID, "steps": steps,
		})
		done <- inputResult{value: value, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			return nil, result.err
		}
		result.value = WithInputDiagnostics(result.value)
		if err := a.journalAt(directory, result.value); err != nil {
			return nil, err
		}
		return result.value, nil
	case <-ctx.Done():
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cancelTimeout)
		cancelResult, cancelErr := a.remote(cleanupCtx, "cancel", map[string]any{
			"lease_id": leaseID, "action_id": actionID,
		})
		cleanupCancel()
		// The target returns from input only after it has released held controls.
		// Waiting here covers a cancellation that arrived before target input.
		result := <-done
		if result.err == nil {
			result.value = WithInputDiagnostics(result.value)
			if err := a.journalAt(directory, result.value); err != nil {
				return result.value, err
			}
		}
		if cancelErr != nil {
			return result.value, fmt.Errorf("input context cancelled and remote cancel failed: %w", cancelErr)
		}
		if result.err != nil {
			return result.value, fmt.Errorf("input context cancelled after remote cancel: %w", result.err)
		}
		_ = cancelResult
		if releaseErrors(result.value) != "" {
			return result.value, fmt.Errorf("%w: %s", ErrCleanupUncertain, releaseErrors(result.value))
		}
		if targetError, _ := result.value["error"].(string); strings.TrimSpace(targetError) != "" {
			return result.value, fmt.Errorf("input context cancelled with target error: %s", targetError)
		}
		return result.value, ctx.Err()
	}
}

func releaseErrors(receipt map[string]any) string {
	if receipt == nil {
		return ""
	}
	raw, ok := receipt["release_errors"]
	if !ok {
		return ""
	}
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return ""
	}
	messages := make([]string, 0, len(values))
	for _, value := range values {
		message := strings.TrimSpace(fmt.Sprint(value))
		if message != "" {
			messages = append(messages, message)
		}
	}
	return strings.Join(messages, "; ")
}

// Cancel asks the target to release the active batch. The action ID is sent
// whenever an input call has reserved one, including before it reaches target.
func (a *Adapter) Cancel(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if err := a.requireLocked(leaseID); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	actionID := a.activeAction
	a.mu.Unlock()
	request := map[string]any{"lease_id": leaseID}
	if actionID != "" {
		request["action_id"] = actionID
	}
	return a.remote(ctx, "cancel", request)
}

// Status returns the target's current lease, session, lobby, and receipt view.
// A non-empty lease ID must be owned by this adapter and renews the target TTL.
func (a *Adapter) Status(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if leaseID != "" {
		if err := a.requireLocked(leaseID); err != nil {
			a.mu.Unlock()
			return nil, err
		}
	}
	a.mu.Unlock()
	request := map[string]any{}
	if leaseID != "" {
		request["lease_id"] = leaseID
	}
	result, err := a.remote(ctx, "status", request)
	if result == nil {
		return nil, err
	}
	return WithStatusDiagnostics(result), err
}

// Release asks the target to cancel input and stop the remote session before
// always tearing down the local Xvfb and Moonlight process groups.
func (a *Adapter) Release(ctx context.Context, leaseID string) (map[string]any, error) {
	a.mu.Lock()
	if a.lease == nil {
		a.mu.Unlock()
		// A service may recover a persisted lease after this capture process has
		// exited. The target still owns the authoritative lease and can safely
		// reject a foreign ID; this adapter never adopts the former local viewer.
		result, err := a.remote(ctx, "release", map[string]any{"lease_id": leaseID})
		if result == nil {
			result = map[string]any{"released": false}
		}
		result["local_capture_stopped"] = false
		result["local_capture"] = "not_owned_after_recovery"
		if err != nil {
			result["target_cleanup"] = "pending target lease expiry"
		}
		return result, err
	}
	if err := a.requireLocked(leaseID); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.closing = true
	directory := a.directory
	a.mu.Unlock()

	result, remoteErr := a.remote(ctx, "release", map[string]any{"lease_id": leaseID})
	if remoteErr != nil {
		result = map[string]any{
			"released": false, "errors": []any{remoteErr.Error()},
			"target_cleanup": "pending target lease expiry",
		}
	}
	a.mu.Lock()
	a.stopCaptureLocked()
	result["local_capture_stopped"] = true
	result["artifact_directory"] = directory
	journalErr := a.journal(result)
	if journalErr != nil {
		result["evidence_error"] = "persist local release receipt: " + journalErr.Error()
	}
	if released, _ := result["released"].(bool); released {
		a.lease, a.directory, a.window, a.env = nil, "", "", nil
		a.closing = false
	}
	a.mu.Unlock()
	if journalErr != nil {
		return result, journalErr
	}
	if remoteErr != nil {
		return result, remoteErr
	}
	return result, nil
}

// Close releases the active lease best-effort, stops the watcher, and unlocks
// the local capture slot. It is safe to call more than once.
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() { a.closeErr = a.close() })
	return a.closeErr
}

func (a *Adapter) close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	leaseID := ""
	if a.lease != nil {
		leaseID, _, _ = leaseIdentity(a.lease)
	}
	a.mu.Unlock()
	if leaseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
		_, _ = a.Release(ctx, leaseID)
		cancel()
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	close(a.stopWatch)
	<-a.watchDone
	a.mu.Lock()
	a.stopCaptureLocked()
	a.mu.Unlock()
	if err := syscall.Flock(int(a.lockFile.Fd()), syscall.LOCK_UN); err != nil {
		_ = a.lockFile.Close()
		return fmt.Errorf("unlock Wolf capture slot: %w", err)
	}
	return a.lockFile.Close()
}

func (a *Adapter) requireLocked(leaseID string) error {
	if a.closed {
		return errors.New("Wolf adapter is closed")
	}
	if a.lease == nil {
		return errors.New("this adapter does not own a Wolf lease")
	}
	id, _, err := leaseIdentity(a.lease)
	if err != nil || id != leaseID {
		return errors.New("this adapter does not own that Wolf lease")
	}
	return nil
}

func (a *Adapter) remote(ctx context.Context, operation string, values map[string]any) (map[string]any, error) {
	if a.remoteCall != nil {
		return a.remoteCall(ctx, operation, values)
	}
	request := make(map[string]any, len(values)+1)
	request["op"] = operation
	for key, value := range values {
		request[key] = value
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode Wolf %s request: %w", operation, err)
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, remoteTimeout)
		defer cancel()
	}
	url := "http://127.0.0.1:" + strconv.Itoa(a.config.TargetPort) + "/command"
	command := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", a.config.SSHHost,
		"curl", "-sS", "--max-time", "25", "-H", "Content-Type:application/json",
		"--data-binary", "@-", url,
	)
	command.Stdin = bytes.NewReader(body)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Wolf %s transport: %w", operation, err)
	}
	return decodeRemoteResponse(operation, output)
}

func decodeRemoteResponse(operation string, output []byte) (map[string]any, error) {
	var response remoteEnvelope
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode Wolf %s response: %w", operation, err)
	}
	if response.OK {
		if response.Result == nil {
			return nil, fmt.Errorf("Wolf %s response omitted result", operation)
		}
		return response.Result, nil
	}
	if strings.TrimSpace(response.Error) == "" {
		return nil, fmt.Errorf("Wolf %s failed without an error", operation)
	}
	return nil, fmt.Errorf("Wolf %s: %s", operation, response.Error)
}

func (a *Adapter) startXvfbLocked(width, height int) error {
	log, err := os.OpenFile(filepath.Join(a.directory, "xvfb.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open Xvfb log: %w", err)
	}
	command := exec.Command("Xvfb", "-displayfd", "1", "-screen", "0", fmt.Sprintf("%dx%dx24", width, height), "-nolisten", "tcp")
	configureProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = log.Close()
		return fmt.Errorf("open Xvfb display output: %w", err)
	}
	command.Stderr = log
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		_ = log.Close()
		return fmt.Errorf("start Xvfb: %w", err)
	}
	a.xvfb = newManagedProcess(command, stdout, log)
	display := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		display <- strings.TrimSpace(line)
	}()
	select {
	case number := <-display:
		if _, err := strconv.Atoi(number); err != nil || number == "" {
			return errors.New("Xvfb failed to allocate a private display; inspect xvfb.log")
		}
		a.env = append(os.Environ(), "DISPLAY=:"+number, "SDL_AUDIODRIVER=dummy", "XDG_CONFIG_HOME="+a.config.MoonlightConfig)
		return nil
	case <-time.After(xvfbStartTimeout):
		return errors.New("Xvfb did not allocate a private display")
	}
}

func (a *Adapter) startMoonlightLocked(width, height, fps int) error {
	log, err := os.OpenFile(filepath.Join(a.directory, "moonlight.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open Moonlight log: %w", err)
	}
	command := exec.Command(a.config.Moonlight, "stream", "--resolution", fmt.Sprintf("%dx%d", width, height),
		"--fps", strconv.Itoa(fps), "--bitrate", strconv.Itoa(a.config.Bitrate),
		"--video-codec", "H.264", "--video-decoder", "software", "--display-mode", "windowed",
		"--audio-config", "stereo",
		"--no-vsync", "--no-frame-pacing", "--no-quit-after", a.config.StreamHost, a.config.Application,
	)
	configureProcess(command)
	command.Env, command.Stdin, command.Stdout, command.Stderr = a.env, nil, log, log
	if err := command.Start(); err != nil {
		_ = log.Close()
		return fmt.Errorf("start Moonlight: %w", err)
	}
	a.moonlight = newManagedProcess(command, nil, log)
	return nil
}

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

func newManagedProcess(command *exec.Cmd, stdout io.ReadCloser, log *os.File) *managedProcess {
	process := &managedProcess{cmd: command, stdout: stdout, log: log, done: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(process.done)
	}()
	return process
}

func (a *Adapter) findMoonlightWindowLocked(ctx context.Context) (string, bool) {
	tree, err := a.runTool(ctx, "xwininfo", "-root", "-tree")
	if err != nil {
		return "", false
	}
	match := moonlightWindow.FindStringSubmatch(tree)
	if len(match) != 2 {
		return "", false
	}
	view, err := a.runTool(ctx, "xwininfo", "-id", match[1])
	if err != nil || !strings.Contains(view, "IsViewable") {
		return "", false
	}
	return match[1], true
}

func (a *Adapter) positionWindowLocked(ctx context.Context, window string, width, height int) error {
	_, err := a.runTool(ctx, "xdotool", "windowsize", window, strconv.Itoa(width), strconv.Itoa(height), "windowmove", window, "0", "0")
	return err
}

func (a *Adapter) runTool(ctx context.Context, name string, args ...string) (string, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, remoteTimeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Env = a.env
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("Wolf capture %s: %w", name, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (a *Adapter) stopCaptureLocked() {
	stopManagedProcess(a.moonlight)
	stopManagedProcess(a.xvfb)
	a.moonlight, a.xvfb = nil, nil
}

func stopManagedProcess(process *managedProcess) {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return
	}
	pid := process.cmd.Process.Pid
	select {
	case <-process.done:
	default:
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(processStopTimeout):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-process.done
		}
	}
	if process.stdout != nil {
		_ = process.stdout.Close()
	}
	if process.log != nil {
		_ = process.log.Close()
	}
}

func processExited(process *managedProcess) bool {
	if process == nil || process.done == nil {
		return true
	}
	select {
	case <-process.done:
		return true
	default:
		return false
	}
}
func (a *Adapter) journal(event map[string]any) error {
	return a.journalAt(a.directory, event)
}

func (a *Adapter) journalAt(directory string, event map[string]any) error {
	if directory == "" {
		return nil
	}
	a.journalMu.Lock()
	defer a.journalMu.Unlock()
	record := make(map[string]any, len(event)+1)
	record["recorded_at_ns"] = time.Now().UnixNano()
	for key, value := range event {
		record[key] = value
	}
	return evidence.AppendJSONFile(filepath.Join(directory, "events.jsonl"), record)
}

func (a *Adapter) watch() {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	defer close(a.watchDone)
	for {
		select {
		case <-a.stopWatch:
			return
		case <-ticker.C:
			a.mu.Lock()
			leaseID, _, _ := leaseIdentity(a.lease)
			exited := a.lease != nil && !a.closing && processExited(a.moonlight)
			a.mu.Unlock()
			if exited && leaseID != "" {
				ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
				_, _ = a.Release(ctx, leaseID)
				cancel()
			}
		}
	}
}

func leaseIdentity(lease map[string]any) (string, string, error) {
	if lease == nil {
		return "", "", errors.New("Wolf target omitted lease")
	}
	id, _ := lease["id"].(string)
	clientID, clientOK := lease["client_id"].(string)
	if strings.TrimSpace(id) == "" || !clientOK || strings.TrimSpace(clientID) == "" {
		return "", "", errors.New("Wolf target returned an incomplete lease")
	}
	return id, clientID, nil
}

func sessionForClient(status map[string]any, clientID string) (map[string]any, bool) {
	sessions, ok := status["sessions"].([]any)
	if !ok {
		return nil, false
	}
	for _, raw := range sessions {
		session, ok := raw.(map[string]any)
		if ok && session["client_id"] == clientID {
			return session, true
		}
	}
	return nil, false
}

func nestedValue(value map[string]any, keys ...string) any {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func mergeEvent(name string, values map[string]any) map[string]any {
	event := make(map[string]any, len(values)+1)
	event["event"] = name
	for key, value := range values {
		event[key] = value
	}
	return event
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
