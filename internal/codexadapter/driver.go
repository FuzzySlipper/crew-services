package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// Capabilities every crew-codex binding advertises. Codex can take a message
// into an active turn (turn/steer), queue one for when a thread is idle
// (thread/queue/add) and load a thread that is not loaded (thread/resume).
var Capabilities = []string{"deliver_next_step", "deliver_when_idle", "durable_next_turn", "wake_inactive"}

// Native is the part of the Codex app-server the driver uses. *StdioAppServer
// connected with DialDaemon implements it.
type Native interface {
	LoadedThreads(ctx context.Context) ([]string, error)
	ReadThread(ctx context.Context, threadID string) (NativeThread, error)
	ActiveTurn(ctx context.Context, threadID string) (string, error)
	RecentTurns(ctx context.Context, threadID string, limit int, withItems bool) ([]NativeTurn, error)
	Steer(ctx context.Context, threadID, turnID, text, clientUserMessageID string) error
	QueueAdd(ctx context.Context, threadID, text, clientUserMessageID string) (QueuedSubmission, error)
	QueueList(ctx context.Context, threadID string) ([]QueuedSubmission, error)
	Resume(ctx context.Context, threadID string) error
}

// Connection is one live connection to the shared daemon.
type Connection interface {
	Native
	Initialize(ctx context.Context) error
	SetNotificationHandler(func(method string, params json.RawMessage))
	Close() error
}

// Fabric is the part of the fabric API the driver uses.
type Fabric interface {
	Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (fabricclient.Lease, error)
	Renew(ctx context.Context, adapterID, token string, duration time.Duration) (fabricclient.Lease, error)
	RegisterSessionAddress(ctx context.Context, request fabricclient.SessionAddressRequest) (fabricclient.Binding, error)
	Resolve(ctx context.Context, address string) (fabricclient.Binding, error)
	Bind(ctx context.Context, address string, request fabricclient.BindRequest) (fabricclient.Binding, error)
	Claim(ctx context.Context, request fabricclient.ClaimRequest) (fabricclient.Claim, error)
	Begin(ctx context.Context, deliveryID string, request fabricclient.DispatchRequest) (fabricclient.Delivery, error)
	Release(ctx context.Context, deliveryID string, request fabricclient.ClaimTokenRequest) (fabricclient.Delivery, error)
	Acknowledge(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error)
	Unknown(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error)
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
}

// target is one fabric address the driver delivers to and the thread behind it.
type target struct {
	Address    string
	ThreadID   string
	Generation int64
	Pinned     bool
}

// knownThread is a thread that has a session address, remembered across
// restarts so its mail can still wake it once the daemon has unloaded it.
type knownThread struct {
	ThreadID string    `json:"thread_id"`
	Address  string    `json:"address"`
	SeenAt   time.Time `json:"seen_at"`
}

// Driver delivers fabric messages into threads of the shared Codex daemon.
type Driver struct {
	fabric Fabric
	now    func() time.Time
	cfg    Config
	log    *slog.Logger

	mu      sync.Mutex
	lease   fabricclient.Lease
	native  Native
	known   map[string]*knownThread // thread id -> session address record
	targets map[string]*target      // address -> target
	skipped map[string]struct{}     // threads that cannot take input
	started []string                // threads announced by thread/started
	read    map[string]NativeThread // latest read of each thread, for who
	passes  int
	pumpMu  sync.Mutex
	wake    chan struct{}
}

// notLoadedEvery is how many passes a thread the daemon has not loaded waits
// between claims: waking one is rare, and each claim is a fabric round trip.
const notLoadedEvery = 15

// forgetAfter drops session-address threads that have not been loaded for
// this long. Their mail still waits in the fabric and is delivered once the
// thread is loaded again.
const forgetAfter = 14 * 24 * time.Hour

func NewDriver(fabric Fabric, now func() time.Time, cfg Config, log *slog.Logger) (*Driver, error) {
	d := &Driver{fabric: fabric, now: now, cfg: cfg, log: log, known: map[string]*knownThread{}, read: map[string]NativeThread{}, targets: map[string]*target{}, skipped: map[string]struct{}{}, wake: make(chan struct{}, 1)}
	if err := d.load(); err != nil {
		return nil, err
	}
	return d, nil
}

// Start takes the adapter lease.
func (d *Driver) Start(ctx context.Context) error {
	lease, err := d.fabric.Register(ctx, d.cfg.AdapterID, d.cfg.InstanceID, d.cfg.LeaseDuration)
	if err != nil {
		return fmt.Errorf("register adapter: %w", err)
	}
	d.mu.Lock()
	d.lease = lease
	d.mu.Unlock()
	return nil
}

func (d *Driver) leaseToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease.LeaseToken
}

func (d *Driver) renew(ctx context.Context) error {
	lease, err := d.fabric.Renew(ctx, d.cfg.AdapterID, d.leaseToken(), d.cfg.LeaseDuration)
	if err != nil {
		lease, err = d.fabric.Register(ctx, d.cfg.AdapterID, d.cfg.InstanceID, d.cfg.LeaseDuration)
		if err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.lease = lease
	d.mu.Unlock()
	return nil
}

// Poke asks the run loop for an immediate pass.
func (d *Driver) Poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Attach makes conn the driver's daemon connection, or detaches with nil.
func (d *Driver) Attach(conn Native) {
	d.mu.Lock()
	d.native = conn
	d.mu.Unlock()
}

// Notify is the daemon connection's notification handler. It only records
// and pokes; the pass does the work.
func (d *Driver) Notify(method string, params json.RawMessage) {
	switch method {
	case "thread/started":
		var body struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(params, &body) == nil && body.Thread.ID != "" {
			d.mu.Lock()
			d.started = append(d.started, body.Thread.ID)
			d.mu.Unlock()
		}
		d.Poke()
	case "thread/status/changed":
		d.Poke()
	}
}

// RegisterThread gives a thread its session address, for the SessionStart
// hook and for threads discovered in the daemon.
func (d *Driver) RegisterThread(ctx context.Context, threadID string) (string, error) {
	if strings.TrimSpace(threadID) == "" {
		return "", errors.New("thread_id is required")
	}
	binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: threadID, Capabilities: Capabilities})
	if err != nil {
		return "", fmt.Errorf("register session address: %w", err)
	}
	d.mu.Lock()
	_, existed := d.known[threadID]
	d.known[threadID] = &knownThread{ThreadID: threadID, Address: binding.Address, SeenAt: d.now().UTC()}
	if current, ok := d.targets[binding.Address]; !ok || current.Generation != binding.Generation {
		d.targets[binding.Address] = &target{Address: binding.Address, ThreadID: threadID, Generation: binding.Generation}
	}
	if !existed {
		err = d.saveLocked()
	}
	d.mu.Unlock()
	if !existed {
		d.log.Info("registered thread", "thread", threadID, "address", binding.Address)
	}
	d.Poke()
	return binding.Address, err
}

// Sessions lists the threads this driver delivers to, with what the last
// pass read about them.
func (d *Driver) Sessions() []crewcli.SessionInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []crewcli.SessionInfo{}
	for _, t := range d.targets {
		info := crewcli.SessionInfo{Address: t.Address, Runtime: "codex", NativeID: t.ThreadID, State: "unknown"}
		if known, ok := d.known[t.ThreadID]; ok {
			info.SeenAt = known.SeenAt
		}
		if thread, ok := d.read[t.ThreadID]; ok {
			info.Cwd = thread.CWD
			info.State = map[string]string{"active": "busy", "idle": "idle", "notLoaded": "not loaded", "systemError": "error"}[thread.Status]
			if info.State == "" {
				info.State = thread.Status
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// Send submits a message from a thread's own session address. The policy
// defaults to next_turn; operationID makes retries of one send idempotent.
func (d *Driver) Send(ctx context.Context, request crewcli.SendRequest) (crewcli.SendResult, error) {
	from, ok := d.Address(request.From)
	if !ok {
		// A thread the daemon has but the driver has not reached yet.
		address, err := d.RegisterThread(ctx, request.From)
		if err != nil {
			return crewcli.SendResult{}, fmt.Errorf("thread %s has no crew address: %w", request.From, err)
		}
		from = address
	}
	if request.To == "" || strings.TrimSpace(request.Body) == "" || request.OperationID == "" {
		return crewcli.SendResult{}, errors.New("to, body and operation_id are required")
	}
	submitted, err := d.fabric.Submit(ctx, fabricclient.MessageRequest{ProducerID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), OperationID: request.OperationID, SenderAddress: from, RecipientAddress: request.To, Body: request.Body, ActivationPolicy: request.Policy, TTL: "24h"})
	if err != nil {
		return crewcli.SendResult{}, err
	}
	return crewcli.SendResult{MessageID: submitted.Message.MessageID, From: from, To: request.To, Replayed: submitted.Replayed}, nil
}

// Address returns a registered thread's session address.
func (d *Driver) Address(threadID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	known, ok := d.known[threadID]
	if !ok {
		return "", false
	}
	return known.Address, true
}

// restoreKnown re-registers the remembered threads after a start, so their
// bindings carry the current capabilities and their generations are known.
func (d *Driver) restoreKnown(ctx context.Context) {
	d.mu.Lock()
	ids := make([]string, 0, len(d.known))
	cutoff := d.now().Add(-forgetAfter)
	for id, known := range d.known {
		if known.SeenAt.Before(cutoff) {
			delete(d.known, id)
			continue
		}
		ids = append(ids, id)
	}
	d.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: id, Capabilities: Capabilities})
		if err != nil {
			d.log.Warn("restore session address failed", "thread", id, "error", err)
			continue
		}
		d.mu.Lock()
		d.targets[binding.Address] = &target{Address: binding.Address, ThreadID: id, Generation: binding.Generation}
		d.mu.Unlock()
	}
}

// ensurePinned keeps each -address mapping bound to this adapter with the
// current capabilities. An address bound to this adapter keeps its target, so
// its generation (and queued work) survives the switch from the old projector.
func (d *Driver) ensurePinned(ctx context.Context) {
	for _, mapping := range d.cfg.Mappings {
		binding, err := d.fabric.Resolve(ctx, mapping.Address)
		if err != nil && fabricclient.Code(err) != "not_found" && !isNotFound(err) {
			d.log.Warn("resolve pinned address failed", "address", mapping.Address, "error", err)
			continue
		}
		if err == nil && binding.Bound && binding.AdapterID != d.cfg.AdapterID {
			d.log.Warn("pinned address is bound to another adapter", "address", mapping.Address, "adapter", binding.AdapterID)
			continue
		}
		if err != nil || !binding.Bound || !sameCapabilities(binding.Capabilities, Capabilities) {
			request := fabricclient.BindRequest{ActorAdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), AdapterID: d.cfg.AdapterID, TargetRef: mapping.ThreadID, Capabilities: Capabilities}
			if err == nil {
				revision := binding.Revision
				request.ExpectedRevision = &revision
				if binding.Bound && binding.TargetRef != "" {
					request.TargetRef = binding.TargetRef
				}
			}
			binding, err = d.fabric.Bind(ctx, mapping.Address, request)
			if err != nil {
				d.log.Warn("bind pinned address failed", "address", mapping.Address, "error", err)
				continue
			}
			d.log.Info("bound pinned address", "address", mapping.Address, "thread", mapping.ThreadID, "generation", binding.Generation)
		}
		d.mu.Lock()
		d.targets[mapping.Address] = &target{Address: mapping.Address, ThreadID: mapping.ThreadID, Generation: binding.Generation, Pinned: true}
		d.mu.Unlock()
	}
}

func isNotFound(err error) bool {
	var httpErr *fabricclient.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == 404
}

func sameCapabilities(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// discover registers threads the daemon has loaded or just started. Threads
// that cannot take input (ephemeral or subagent threads) are skipped.
func (d *Driver) discover(ctx context.Context, native Native, loaded []string) {
	d.mu.Lock()
	candidates := append(d.started, loaded...)
	d.started = nil
	d.mu.Unlock()
	for _, id := range candidates {
		d.mu.Lock()
		known, isKnown := d.known[id]
		_, skip := d.skipped[id]
		registered := false
		if isKnown {
			if t, ok := d.targets[known.Address]; ok && t.ThreadID == id {
				registered = true
			}
			if d.now().Sub(known.SeenAt) > time.Hour {
				known.SeenAt = d.now().UTC()
				_ = d.saveLocked()
			}
		}
		d.mu.Unlock()
		if registered || skip {
			continue
		}
		if !isKnown {
			thread, err := native.ReadThread(ctx, id)
			if err != nil {
				d.log.Warn("read new thread failed", "thread", id, "error", err)
				continue
			}
			if thread.Ephemeral || !thread.AcceptsInput {
				d.mu.Lock()
				d.skipped[id] = struct{}{}
				d.mu.Unlock()
				continue
			}
		}
		if _, err := d.RegisterThread(ctx, id); err != nil {
			d.log.Warn("register thread failed", "thread", id, "error", err)
		}
	}
}

// Run renews the lease and delivers until ctx ends. connect opens a daemon
// connection; a failed or lost connection is retried every interval.
func (d *Driver) Run(ctx context.Context, interval time.Duration, connect func(context.Context) (Connection, error)) error {
	d.ensurePinned(ctx)
	d.restoreKnown(ctx)
	renew := time.NewTicker(d.cfg.LeaseDuration / 3)
	defer renew.Stop()
	pump := time.NewTicker(interval)
	defer pump.Stop()
	var conn Connection
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	for {
		if conn == nil {
			c, err := connect(ctx)
			if err == nil {
				c.SetNotificationHandler(d.Notify)
				initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				if err = c.Initialize(initCtx); err != nil {
					_ = c.Close()
				}
				cancel()
			}
			if err != nil {
				d.log.Warn("Codex daemon unavailable", "error", err)
			} else {
				conn = c
				d.Attach(c)
				d.log.Info("connected to Codex daemon")
			}
		}
		if conn != nil {
			if err := d.Pump(ctx); err != nil && ctx.Err() == nil {
				d.log.Warn("delivery pass failed; reconnecting", "error", err)
				d.Attach(nil)
				_ = conn.Close()
				conn = nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-renew.C:
			if err := d.renew(ctx); err != nil {
				d.log.Warn("lease renewal failed", "error", err)
			}
			d.ensurePinned(ctx)
		case <-pump.C:
		case <-d.wake:
		}
	}
}

// Pump makes one pass: discover threads, then drain each target's eligible
// work in order. It returns an error only when the daemon connection failed.
func (d *Driver) Pump(ctx context.Context) error {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	d.mu.Lock()
	native := d.native
	d.passes++
	pass := d.passes
	d.mu.Unlock()
	if native == nil {
		return errors.New("no Codex daemon connection")
	}
	loadedIDs, err := native.LoadedThreads(ctx)
	if err != nil {
		return fmt.Errorf("list loaded threads: %w", err)
	}
	loaded := make(map[string]bool, len(loadedIDs))
	for _, id := range loadedIDs {
		loaded[id] = true
	}
	d.discover(ctx, native, loadedIDs)
	statuses := map[string]string{}
	for _, t := range d.snapshot() {
		if !loaded[t.ThreadID] && pass%notLoadedEvery != 1 {
			continue
		}
		status, ok := statuses[t.ThreadID]
		if !ok {
			thread, err := native.ReadThread(ctx, t.ThreadID)
			if err != nil {
				if !IsRejection(err) {
					return fmt.Errorf("read thread %s: %w", t.ThreadID, err)
				}
				d.log.Warn("read thread failed", "thread", t.ThreadID, "address", t.Address, "error", err)
				statuses[t.ThreadID] = ""
				continue
			}
			status = thread.Status
			statuses[t.ThreadID] = status
			d.mu.Lock()
			d.read[t.ThreadID] = thread
			d.mu.Unlock()
		}
		availability := availabilityOf(status)
		if availability == "" {
			continue
		}
		for i := 0; i < 20; i++ {
			delivered, err := d.deliverOne(ctx, native, t, availability)
			if err != nil {
				d.log.Warn("delivery failed", "thread", t.ThreadID, "address", t.Address, "error", err)
			}
			if !delivered {
				break
			}
			// What was just delivered may have started a turn.
			if thread, err := native.ReadThread(ctx, t.ThreadID); err == nil {
				statuses[t.ThreadID] = thread.Status
				availability = availabilityOf(thread.Status)
				if availability == "" {
					break
				}
			}
		}
	}
	return nil
}

func availabilityOf(status string) string {
	switch status {
	case "active":
		return "busy"
	case "idle":
		return "idle"
	case "notLoaded":
		return "inactive"
	}
	// systemError or unknown: leave the work queued.
	return ""
}

func (d *Driver) snapshot() []target {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]target, 0, len(d.targets))
	for _, t := range d.targets {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

const attemptPrefix = "codex:"

func nativeAttempt(deliveryID string) string { return attemptPrefix + deliveryID }

// ClientMessageID is the clientUserMessageId a delivery carries into Codex.
// Codex records it on the user message, so a lost response is reconciled by
// looking for it in the queue and in recent turns.
func ClientMessageID(deliveryID string) string { return "crew-delivery:" + deliveryID }

func opID(kind, subject string) string { return "crew-codex/" + kind + "/" + subject }

// deliverOne claims and delivers at most one message for t. It reports
// whether a message went out.
func (d *Driver) deliverOne(ctx context.Context, native Native, t target, availability string) (bool, error) {
	token := d.leaseToken()
	now := d.now().UTC()
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: fmt.Sprintf("crew-codex/claim/%s/%d", t.Address, now.UnixNano()), RecipientAddress: t.Address, RecipientGeneration: t.Generation, Availability: availability, ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		if claim.Reason == "head_dispatching" && claim.Head != nil {
			return false, d.reconcileStuck(ctx, native, t, *claim.Head)
		}
		return false, nil
	}
	delivery := claim.Delivery
	action := claim.DispatchAction
	if action == "" {
		action = delivery.DispatchAction
	}
	// Read what the dispatch needs before recording it: if the daemon is gone,
	// the claim goes back to the queue instead of becoming unknown.
	activeTurn := ""
	if action == "deliver_next_step" {
		activeTurn, err = native.ActiveTurn(ctx, t.ThreadID)
	}
	if err != nil {
		if _, releaseErr := d.fabric.Release(ctx, delivery.DeliveryID, fabricclient.ClaimTokenRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: fmt.Sprintf("crew-codex/release/%s/%d", delivery.DeliveryID, now.UnixNano()), ClaimToken: claim.ClaimToken}); releaseErr != nil {
			return false, fmt.Errorf("release after %v: %w", err, releaseErr)
		}
		return false, fmt.Errorf("read active turn: %w", err)
	}
	ref := nativeAttempt(delivery.DeliveryID)
	if _, err := d.fabric.Begin(ctx, delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", delivery.DeliveryID), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref}); err != nil {
		return false, fmt.Errorf("begin dispatch: %w", err)
	}
	clientID := ClientMessageID(delivery.DeliveryID)
	text := Render(*claim.Message)
	how, err := d.dispatch(ctx, native, t.ThreadID, action, activeTurn, text, clientID)
	if err != nil {
		// The native outcome is ambiguous: look for the message before
		// settling, and never send it again.
		accepted, checkErr := nativeHas(ctx, native, t.ThreadID, clientID)
		if checkErr != nil || !accepted {
			if _, unknownErr := d.fabric.Unknown(ctx, delivery.DeliveryID, fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("unknown", delivery.DeliveryID), NativeAttemptRef: ref}); unknownErr != nil {
				return false, fmt.Errorf("outcome unknown after %v: %w", err, unknownErr)
			}
			return false, fmt.Errorf("%s: %w", action, err)
		}
		how = "reconciled"
	}
	if _, err := d.fabric.Acknowledge(ctx, delivery.DeliveryID, fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("ack", delivery.DeliveryID), NativeAttemptRef: ref}); err != nil {
		return true, fmt.Errorf("acknowledge: %w", err)
	}
	d.log.Info("delivered", "thread", t.ThreadID, "address", t.Address, "delivery", delivery.DeliveryID, "action", action, "native", how)
	if action == "wake_then_deliver" {
		// The queued item is on disk; loading the thread lets Codex start it.
		if err := native.Resume(ctx, t.ThreadID); err != nil {
			d.log.Warn("resume after queueing failed; the item runs when the thread is next loaded", "thread", t.ThreadID, "error", err)
		}
	}
	return true, nil
}

// dispatch makes the one native call for a claimed delivery and says how the
// message went in. An error means the outcome is unknown; a request Codex
// refused before the message got in falls back where that is safe.
func (d *Driver) dispatch(ctx context.Context, native Native, threadID, action, activeTurn, text, clientID string) (string, error) {
	if action == "deliver_next_step" && activeTurn != "" {
		err := native.Steer(ctx, threadID, activeTurn, text, clientID)
		if err == nil {
			return "steered", nil
		}
		if !IsRejection(err) {
			return "", err
		}
		// The turn ended (or changed) in the meantime: the steer had no
		// effect, so the next turn takes it instead.
	}
	queued, err := native.QueueAdd(ctx, threadID, text, clientID)
	if err != nil {
		return "", err
	}
	if queued.ClientUserMessageID != "" && queued.ClientUserMessageID != clientID {
		return "", fmt.Errorf("thread/queue/add returned client id %q", queued.ClientUserMessageID)
	}
	return "queued", nil
}

// reconcileStuck settles this driver's own dispatch that a crash or lost
// response left in flight, from what Codex holds.
func (d *Driver) reconcileStuck(ctx context.Context, native Native, t target, head fabricclient.Delivery) error {
	if head.ClaimOwnerAdapterID != "" && head.ClaimOwnerAdapterID != d.cfg.AdapterID {
		return nil
	}
	if head.NativeAttemptRef == "" || !(strings.HasPrefix(head.NativeAttemptRef, attemptPrefix) || strings.HasPrefix(head.NativeAttemptRef, "codex-queue:")) {
		return nil
	}
	clientID := ClientMessageID(head.DeliveryID)
	accepted, err := nativeHas(ctx, native, t.ThreadID, clientID)
	if err != nil {
		return fmt.Errorf("reconcile %s: %w", head.DeliveryID, err)
	}
	request := fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeAttemptRef: head.NativeAttemptRef}
	if accepted {
		request.OperationID = opID("ack", head.DeliveryID)
		_, err = d.fabric.Acknowledge(ctx, head.DeliveryID, request)
	} else {
		request.OperationID = opID("unknown", head.DeliveryID)
		_, err = d.fabric.Unknown(ctx, head.DeliveryID, request)
	}
	if err == nil {
		d.log.Info("reconciled in-flight delivery", "thread", t.ThreadID, "delivery", head.DeliveryID, "accepted", accepted)
	}
	return err
}

// nativeHas reports whether Codex holds the message with clientID, either
// still queued or already in one of the thread's recent turns.
func nativeHas(ctx context.Context, native Native, threadID, clientID string) (bool, error) {
	queued, queueErr := native.QueueList(ctx, threadID)
	if queueErr == nil {
		for _, item := range queued {
			if item.ClientUserMessageID == clientID {
				return true, nil
			}
		}
	}
	turns, err := native.RecentTurns(ctx, threadID, 5, true)
	if err != nil {
		return false, err
	}
	for _, turn := range turns {
		if slices.Contains(turn.ClientIDs, clientID) {
			return true, nil
		}
	}
	if queueErr != nil {
		return false, queueErr
	}
	return false, nil
}

// Render is the text the Codex thread reads, as a user message. It says
// where the message came from so the agent treats it as crew traffic.
func Render(m fabricclient.Message) string {
	return fmt.Sprintf("Crew message from %s (crew message %s):\n\n%s", m.SenderAddress, m.MessageID, strings.TrimSpace(m.Body))
}

func (d *Driver) load() error {
	if d.cfg.StatePath == "" {
		return nil
	}
	data, err := os.ReadFile(d.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read driver state: %w", err)
	}
	var stored struct {
		Threads []*knownThread `json:"threads"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		// The pre-daemon adapter kept a different shape here; start empty.
		d.log.Warn("ignoring unreadable driver state", "path", d.cfg.StatePath, "error", err)
		return nil
	}
	for _, known := range stored.Threads {
		if known != nil && known.ThreadID != "" {
			d.known[known.ThreadID] = known
		}
	}
	return nil
}

func (d *Driver) saveLocked() error {
	if d.cfg.StatePath == "" {
		return nil
	}
	list := make([]*knownThread, 0, len(d.known))
	for _, known := range d.known {
		list = append(list, known)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ThreadID < list[j].ThreadID })
	data, err := json.MarshalIndent(struct {
		Threads []*knownThread `json:"threads"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.cfg.StatePath), 0o700); err != nil {
		return err
	}
	tmp := d.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.cfg.StatePath)
}
