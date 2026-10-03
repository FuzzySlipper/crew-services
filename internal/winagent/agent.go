// Package winagent runs product instances on the Windows playtest box and
// brokers its one foreground. It runs inside the interactive console session,
// so windows, capture and OS input are those a person at the box would see.
//
// Two tiers share the box. Engine-tier sessions drive each instance over its
// own HTTP surface (crew-services' engine backend), concurrently and without
// focus. OS-tier input (SendInput) only reaches the foreground window, so it
// is serialized by a lease: one holder at a time, bounded, with held input
// released when the lease ends. Uncertain input is never replayed.
package winagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"crew-services/internal/playtest/input"
)

// Product is one launchable product build on the box.
type Product struct {
	// Repo is the product checkout; Project its C# project, relative to Repo.
	Repo    string `json:"repo"`
	Project string `json:"project"`
	// Args are extra `rusty dev` arguments.
	Args []string `json:"args,omitempty"`
	// Runtime is a runtime pack to run instead of the Engine pair the
	// product pins (--runtime). Without one, each start installs the pinned
	// pair (`rusty install`, a no-op once cached) and runs on it.
	Runtime string `json:"runtime,omitempty"`
	// Lanes is how many instances may run at once (default 1). rusty dev
	// stages the product inside its checkout, and Windows cannot replace
	// files a running host has open, so each further lane is a git worktree
	// of Repo beside it (Repo-lane2, ...), moved to Repo's HEAD at each start.
	Lanes int `json:"lanes,omitempty"`
}

// Config is the agent's machine configuration (agent.json beside the binary).
type Config struct {
	Listen string `json:"listen"`
	// BindHost is the LAN address instances serve on, so crew reaches them.
	BindHost string `json:"bind_host"`
	// Ports instances are given, first..last inclusive.
	FirstPort int `json:"first_port"`
	LastPort  int `json:"last_port"`
	// Rusty is rusty.exe. It delegates to each product's pinned pair.
	Rusty string `json:"rusty"`
	// Runtime is the runtime pack for products that name none; empty runs
	// each product on its pinned pair.
	Runtime string             `json:"runtime,omitempty"`
	Logs    string             `json:"logs"`
	Env     map[string]string  `json:"env,omitempty"`
	Product map[string]Product `json:"products"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" || c.BindHost == "" || c.Rusty == "" || c.Logs == "" || c.FirstPort < 1 || c.LastPort < c.FirstPort || len(c.Product) == 0 {
		return c, fmt.Errorf("%s needs listen, bind_host, rusty, logs, first_port..last_port and products", path)
	}
	return c, nil
}

// Desktop is what the agent needs from Windows. Tests use a fake.
type Desktop interface {
	// Start launches a process tree; Stop ends the whole tree.
	Start(command string, args []string, dir string, env []string, log string) (pid int, err error)
	Stop(pid int) error
	Alive(pid int) bool
	// Image is the executable name of a running process ("rusty.exe").
	Image(pid int) string
	// Window finds the visible top-level window of pid's process tree.
	Window(pid int) (uintptr, error)
	CapturePNG(window uintptr) ([]byte, error)
	// CaptureDesktopPNG captures the whole primary screen as shown.
	CaptureDesktopPNG() ([]byte, error)
	// Foreground brings window to the front and reports whether it is.
	Foreground(window uintptr) (bool, error)
	// Send delivers OS input; Release lifts every control Send left held.
	// A point step lands at that position in window's capture, or in the
	// desktop capture when window is 0.
	Send(steps []map[string]any, window uintptr) (map[string]any, error)
	Release() error
	Facts() map[string]any
}

type Instance struct {
	ID        string    `json:"id"`
	Product   string    `json:"product"`
	Port      int       `json:"port"`
	Origin    string    `json:"origin"`
	PID       int       `json:"pid"`
	Log       string    `json:"log"`
	StartedAt time.Time `json:"started_at"`
	Holder    string    `json:"holder,omitempty"`
	// Lane is the checkout this instance runs from.
	Lane string `json:"lane"`
}

type Lease struct {
	ID       string    `json:"id"`
	Holder   string    `json:"holder"`
	Instance string    `json:"instance"`
	Expires  time.Time `json:"expires"`
}

type Agent struct {
	config  Config
	desktop Desktop
	now     func() time.Time
	probe   func(ctx context.Context, origin string, alive func() bool) error
	// prepare makes a lane checkout current before rusty dev runs in it.
	prepare func(ctx context.Context, repo, lane string, env []string, log string) error
	// install installs a lane's pinned Engine pair, for products run on it.
	install func(ctx context.Context, rusty, lane string, env []string, log string) error

	mu        sync.Mutex
	instances map[string]*Instance
	lease     *Lease
	next      int
	inputMu   sync.Mutex
	// starting serializes starts per checkout: rusty dev builds and stages
	// into it until the host answers.
	starting map[string]*sync.Mutex
}

func New(config Config, desktop Desktop) *Agent {
	return &Agent{config: config, desktop: desktop, now: time.Now, probe: probeHost, prepare: prepareLane, install: installPair, instances: map[string]*Instance{}, starting: map[string]*sync.Mutex{}}
}

// probeHost waits for a product host's live-debug catalog to answer, or for
// its process to end (a failed build exits rusty dev).
func probeHost(ctx context.Context, origin string, alive func() bool) error {
	for {
		if !alive() {
			return errors.New("rusty dev exited before the host answered")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/__rusty/product/runtime/debug/catalog", nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance did not answer at %s: %w", origin, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// prepareLane creates or updates a worktree lane at the repository's HEAD,
// with its UI dependencies installed. Lane 1 is the repository itself.
func prepareLane(ctx context.Context, repo, lane string, env []string, log string) error {
	if lane == repo {
		return nil
	}
	file, err := os.Create(log)
	if err != nil {
		return err
	}
	defer file.Close()
	run := func(dir, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, file, file
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %w (log %s)", name, strings.Join(args, " "), err, log)
		}
		return nil
	}
	head, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read %s HEAD: %w", repo, err)
	}
	if _, err := os.Stat(lane); errors.Is(err, os.ErrNotExist) {
		if err := run(repo, "git", "worktree", "add", "--detach", lane, strings.TrimSpace(string(head))); err != nil {
			return err
		}
	} else if err := run(lane, "git", "checkout", "--detach", "--force", strings.TrimSpace(string(head))); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(lane, "pnpm-lock.yaml")); err == nil {
		return run(lane, "pnpm", "install", "--frozen-lockfile", "--prefer-offline", "--reporter=append-only")
	}
	return nil
}

// installPair installs the Engine pair the checkout pins; rusty skips one
// already in its cache.
func installPair(ctx context.Context, rusty, lane string, env []string, log string) error {
	file, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	cmd := exec.CommandContext(ctx, rusty, "install")
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = lane, env, file, file
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rusty install: %w (log %s)", err, log)
	}
	return nil
}

func laneDir(p Product, lane int) string {
	if lane == 1 {
		return p.Repo
	}
	return fmt.Sprintf("%s-lane%d", strings.TrimRight(p.Repo, `\/`), lane)
}

func (a *Agent) freePort() (int, error) {
	used := map[int]bool{}
	for _, instance := range a.instances {
		used[instance.Port] = true
	}
	for port := a.config.FirstPort; port <= a.config.LastPort; port++ {
		if used[port] {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(a.config.BindHost, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		listener.Close()
		return port, nil
	}
	return 0, errors.New("no free instance port")
}

// StartInstance launches one product build in its own window and port, and
// waits until its host answers. holder names the session that owns it.
func (a *Agent) StartInstance(ctx context.Context, product, holder string) (*Instance, error) {
	p, ok := a.config.Product[product]
	if !ok {
		return nil, fmt.Errorf("unknown product %q", product)
	}
	a.mu.Lock()
	lanes := max(1, p.Lanes)
	busy := map[string]bool{}
	for _, other := range a.instances {
		busy[other.Lane] = true
	}
	lane := ""
	for i := 1; i <= lanes && lane == ""; i++ {
		if !busy[laneDir(p, i)] {
			lane = laneDir(p, i)
		}
	}
	if lane == "" {
		a.mu.Unlock()
		return nil, fmt.Errorf("all %d lanes of %s are running instances; stop one or configure more lanes", lanes, product)
	}
	port, err := a.freePort()
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	id := ""
	for id == "" || a.instances[id] != nil {
		a.next++
		id = fmt.Sprintf("%s-%d-%d", product, port, a.next)
	}
	instance := &Instance{ID: id, Product: product, Port: port, Origin: fmt.Sprintf("http://%s:%d", a.config.BindHost, port),
		Log: filepath.Join(a.config.Logs, id+".log"), StartedAt: a.now().UTC(), Holder: holder, Lane: lane}
	a.instances[id] = instance // reserves the port and the lane
	checkout := a.starting[lane]
	if checkout == nil {
		checkout = &sync.Mutex{}
		a.starting[lane] = checkout
	}
	a.mu.Unlock()
	// One start at a time per lane: rusty dev builds and stages into it.
	checkout.Lock()
	defer checkout.Unlock()
	env := os.Environ()
	for key, value := range a.config.Env {
		env = append(env, key+"="+value)
	}
	runtime := p.Runtime
	if runtime == "" {
		runtime = a.config.Runtime
	}
	prepareLog := filepath.Join(a.config.Logs, id+".prepare.log")
	if err := a.prepare(ctx, p.Repo, lane, env, prepareLog); err != nil {
		a.forget(id)
		return nil, fmt.Errorf("prepare lane %s: %w", lane, err)
	}
	if runtime == "" {
		if err := a.install(ctx, a.config.Rusty, lane, env, prepareLog); err != nil {
			a.forget(id)
			return nil, fmt.Errorf("install the Engine pair %s pins: %w", product, err)
		}
	}
	args := []string{"dev", "--project", filepath.Join(lane, p.Project), "--output", "window", "--bind-host", a.config.BindHost,
		"--port", strconv.Itoa(port), "--live-debug", "--diagnostics-log", filepath.Join(a.config.Logs, id+".ndjson")}
	if runtime != "" {
		args = append(args, "--runtime", runtime)
	}
	args = append(args, p.Args...)
	pid, err := a.desktop.Start(a.config.Rusty, args, lane, env, instance.Log)
	if err != nil {
		a.forget(id)
		return nil, err
	}
	a.mu.Lock()
	instance.PID = pid
	a.mu.Unlock()
	a.record()
	ready, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := a.probe(ready, instance.Origin, func() bool { return a.desktop.Alive(pid) }); err != nil {
		if stopErr := a.desktop.Stop(pid); stopErr != nil && a.desktop.Alive(pid) {
			// Still running: keep it listed and recorded, and report it, so
			// the caller can stop it again.
			a.mu.Lock()
			copy := *instance
			a.mu.Unlock()
			return &copy, fmt.Errorf("%w; log %s; stopping it failed, instance %s still running: %v", err, instance.Log, id, stopErr)
		}
		a.forget(id)
		return nil, fmt.Errorf("%w; log %s", err, instance.Log)
	}
	copy := *instance
	return &copy, nil
}

func (a *Agent) forget(id string) {
	a.mu.Lock()
	delete(a.instances, id)
	a.mu.Unlock()
	a.record()
}

func (a *Agent) recordPath() string { return filepath.Join(a.config.Logs, "instances.json") }

// record keeps the started instances on disk, so a restarted agent can stop
// the ones its previous run left behind.
func (a *Agent) record() {
	a.mu.Lock()
	started := map[string]Instance{}
	for id, instance := range a.instances {
		if instance.PID != 0 {
			started[id] = *instance
		}
	}
	a.mu.Unlock()
	data, _ := json.Marshal(started)
	_ = os.WriteFile(a.recordPath(), data, 0o644)
}

// ReapLeftovers stops instances a previous agent run started and never
// stopped. A recorded process ID now used by another program is left alone;
// one that would not stop is kept as an instance, so it can be stopped again.
func (a *Agent) ReapLeftovers() []string {
	data, err := os.ReadFile(a.recordPath())
	if err != nil {
		return nil
	}
	recorded := map[string]Instance{}
	if json.Unmarshal(data, &recorded) != nil {
		// Records from before whole instances were kept hold process IDs.
		var pids map[string]int
		if json.Unmarshal(data, &pids) != nil {
			return nil
		}
		for id, pid := range pids {
			recorded[id] = Instance{ID: id, PID: pid}
		}
	}
	var stopped []string
	for id, instance := range recorded {
		// Later ids never repeat a recorded one.
		if n, err := strconv.Atoi(id[strings.LastIndex(id, "-")+1:]); err == nil {
			a.next = max(a.next, n)
		}
		pid := instance.PID
		if a.desktop.Alive(pid) && strings.EqualFold(a.desktop.Image(pid), filepath.Base(a.config.Rusty)) {
			if a.desktop.Stop(pid) == nil || !a.desktop.Alive(pid) {
				stopped = append(stopped, id)
			} else {
				// Still running: it keeps its port and lane until stopped.
				instance.ID = id
				a.mu.Lock()
				a.instances[id] = &instance
				a.mu.Unlock()
			}
		}
	}
	a.record()
	return stopped
}

func (a *Agent) StopInstance(id string) error {
	a.mu.Lock()
	instance, ok := a.instances[id]
	var leaseID string
	if a.lease != nil && a.lease.Instance == id {
		leaseID = a.lease.ID
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown instance %q", id)
	}
	if leaseID != "" {
		_ = a.EndLease(leaseID)
	}
	if err := a.desktop.Stop(instance.PID); err != nil && a.desktop.Alive(instance.PID) {
		return err
	}
	a.forget(id)
	return nil
}

func (a *Agent) Instances() []Instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Instance, 0, len(a.instances))
	for _, instance := range a.instances {
		if instance.PID != 0 && !a.desktop.Alive(instance.PID) {
			continue
		}
		out = append(out, *instance)
	}
	return out
}

func (a *Agent) instance(id string) (*Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	instance, ok := a.instances[id]
	if !ok || instance.PID == 0 {
		return nil, fmt.Errorf("unknown instance %q", id)
	}
	copy := *instance
	return &copy, nil
}

// Capture returns the instance window as Windows composes it: world and UI
// overlay together. The window must not be minimized; it need not be in front.
func (a *Agent) Capture(id string) ([]byte, error) {
	instance, err := a.instance(id)
	if err != nil {
		return nil, err
	}
	window, err := a.desktop.Window(instance.PID)
	if err != nil {
		return nil, err
	}
	return a.desktop.CapturePNG(window)
}

// CaptureDesktop captures the whole screen: every window, dialog and the
// taskbar, as a person at the box would see them.
func (a *Agent) CaptureDesktop() ([]byte, error) { return a.desktop.CaptureDesktopPNG() }

// TakeLease grants the foreground to holder for ttl, or refuses while
// another holder's lease is live. A holder renewing keeps its lease. An
// empty instance leases the desktop itself: input goes wherever it lands,
// and no window is brought to the front.
func (a *Agent) TakeLease(holder, instance string, ttl time.Duration) (*Lease, error) {
	if holder == "" || ttl <= 0 || ttl > 10*time.Minute {
		return nil, errors.New("a lease needs a holder and a ttl of up to 10 minutes")
	}
	if instance != "" {
		if _, err := a.instance(instance); err != nil {
			return nil, err
		}
	}
	a.expire()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lease != nil && a.lease.Holder != holder {
		return nil, fmt.Errorf("foreground_busy: held by %s until %s", a.lease.Holder, a.lease.Expires.Format(time.RFC3339))
	}
	a.next++
	a.lease = &Lease{ID: fmt.Sprintf("lease-%d", a.next), Holder: holder, Instance: instance, Expires: a.now().Add(ttl)}
	copy := *a.lease
	return &copy, nil
}

// expire ends a lapsed lease, releasing anything its input left held.
func (a *Agent) expire() {
	a.mu.Lock()
	lapsed := a.lease != nil && !a.now().Before(a.lease.Expires)
	if lapsed {
		a.lease = nil
	}
	a.mu.Unlock()
	if lapsed {
		a.inputMu.Lock()
		_ = a.desktop.Release()
		a.inputMu.Unlock()
	}
}

func (a *Agent) EndLease(id string) error {
	a.mu.Lock()
	if a.lease == nil || a.lease.ID != id {
		a.mu.Unlock()
		return fmt.Errorf("lease %q is not current", id)
	}
	a.lease = nil
	a.mu.Unlock()
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	return a.desktop.Release()
}

// LeaseInput brings the leased instance to the front and sends OS input. The
// batch must finish inside the lease. A failure part-way releases what is
// held and reports the outcome as uncertain; it is never replayed.
func (a *Agent) LeaseInput(id string, steps []map[string]any) (map[string]any, error) {
	if err := input.ValidateBatch(steps); err != nil {
		return nil, err
	}
	a.expire()
	a.mu.Lock()
	lease := a.lease
	if lease == nil || lease.ID != id {
		a.mu.Unlock()
		return nil, fmt.Errorf("lease %q is not current", id)
	}
	if total := batchMS(steps); a.now().Add(time.Duration(total) * time.Millisecond).After(lease.Expires) {
		a.mu.Unlock()
		return nil, fmt.Errorf("the batch (%d ms) would outlast the lease; renew it first", total)
	}
	instanceID := lease.Instance
	a.mu.Unlock()
	var window uintptr
	if instanceID != "" {
		instance, err := a.instance(instanceID)
		if err != nil {
			return nil, err
		}
		if window, err = a.desktop.Window(instance.PID); err != nil {
			return nil, err
		}
	}
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	if window != 0 {
		front, err := a.desktop.Foreground(window)
		if err != nil || !front {
			return map[string]any{"delivery": "not-sent", "foreground": false}, fmt.Errorf("could not bring the instance to the foreground: %v", err)
		}
	}
	receipt, err := a.desktop.Send(steps, window)
	if receipt == nil {
		receipt = map[string]any{}
	}
	receipt["tier"] = "os"
	receipt["input_layers"] = "SendInput -> Windows focus -> winit window events -> desktop shell input path (pointer lock, page input capture)"
	if window == 0 {
		receipt["input_layers"] = "SendInput -> whatever window Windows has in front (desktop lease)"
	}
	if err != nil {
		releaseErr := a.desktop.Release()
		receipt["delivery"] = "uncertain; reobserve without replay"
		if releaseErr != nil {
			receipt["release_error"] = releaseErr.Error()
		}
		return receipt, err
	}
	receipt["delivery"] = "sent"
	return receipt, nil
}

func batchMS(steps []map[string]any) int {
	total := 0
	for _, step := range steps {
		total += number(step["ms"])
	}
	return total
}

func number(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func (a *Agent) Status() map[string]any {
	a.expire()
	a.mu.Lock()
	var lease any
	if a.lease != nil {
		copy := *a.lease
		lease = copy
	}
	a.mu.Unlock()
	products := make([]string, 0, len(a.config.Product))
	for name := range a.config.Product {
		products = append(products, name)
	}
	return map[string]any{"desktop": a.desktop.Facts(), "instances": a.Instances(), "lease": lease, "products": products,
		"ports": fmt.Sprintf("%d-%d", a.config.FirstPort, a.config.LastPort)}
}

// Handler serves the agent's HTTP API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, value any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			status := http.StatusUnprocessableEntity
			if strings.HasPrefix(err.Error(), "foreground_busy") {
				status = http.StatusConflict
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "result": value})
			return
		}
		json.NewEncoder(w).Encode(value)
	}
	decode := func(r *http.Request, target any) error {
		decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		return decoder.Decode(target)
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { reply(w, a.Status(), nil) })
	mux.HandleFunc("GET /v1/instances", func(w http.ResponseWriter, r *http.Request) { reply(w, a.Instances(), nil) })
	mux.HandleFunc("POST /v1/instances", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Product string `json:"product"`
			Holder  string `json:"holder"`
		}
		if err := decode(r, &body); err != nil {
			reply(w, nil, err)
			return
		}
		instance, err := a.StartInstance(r.Context(), body.Product, body.Holder)
		reply(w, instance, err)
	})
	mux.HandleFunc("DELETE /v1/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := a.StopInstance(r.PathValue("id"))
		reply(w, map[string]any{"stopped": err == nil}, err)
	})
	mux.HandleFunc("GET /v1/desktop.png", func(w http.ResponseWriter, r *http.Request) {
		png, err := a.CaptureDesktop()
		if err != nil {
			reply(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})
	mux.HandleFunc("GET /v1/instances/{id}/window.png", func(w http.ResponseWriter, r *http.Request) {
		png, err := a.Capture(r.PathValue("id"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})
	mux.HandleFunc("POST /v1/lease", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Holder   string `json:"holder"`
			Instance string `json:"instance"`
			TTLMS    int    `json:"ttl_ms"`
		}
		if err := decode(r, &body); err != nil {
			reply(w, nil, err)
			return
		}
		lease, err := a.TakeLease(body.Holder, body.Instance, time.Duration(body.TTLMS)*time.Millisecond)
		reply(w, lease, err)
	})
	mux.HandleFunc("POST /v1/lease/{id}/input", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Steps []map[string]any `json:"steps"`
		}
		if err := decode(r, &body); err != nil {
			reply(w, nil, err)
			return
		}
		receipt, err := a.LeaseInput(r.PathValue("id"), body.Steps)
		reply(w, receipt, err)
	})
	mux.HandleFunc("DELETE /v1/lease/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := a.EndLease(r.PathValue("id"))
		reply(w, map[string]any{"released": err == nil}, err)
	})
	return mux
}

// Close stops every instance the agent started.
func (a *Agent) Close() {
	for _, instance := range a.Instances() {
		_ = a.StopInstance(instance.ID)
	}
}
