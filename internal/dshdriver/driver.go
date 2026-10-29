package dshdriver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// Capabilities every crew-dsh binding advertises. session/prompt mode:'steer'
// reaches a running agent at its next step boundary, and the dsh-crew-inject
// plugin's agent.inject does so without ever waking it; mode:'queue' becomes
// the next turn, and the host persists the inbox in the session log; and the
// host resumes a cold session before admitting a prompt.
var Capabilities = []string{"deliver_next_step", "deliver_when_idle", "durable_next_turn", "wake_inactive"}

// Native is the part of the DSH web host the driver uses. *WebHost
// implements it.
type Native interface {
	List(ctx context.Context) ([]SessionSummary, error)
	State(ctx context.Context, sessionID string) (SessionState, error)
	Events(ctx context.Context, sessionID string, maxMessages int) ([]Event, error)
	Prompt(ctx context.Context, p PromptRequest) error
	// Inject inserts into the next-step inbox without waking the agent (the
	// dsh-crew-inject plugin); see WebHost.Inject.
	Inject(ctx context.Context, p PromptRequest) error
}

// Fabric is the part of the fabric API the driver uses.
type Fabric interface {
	Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (fabricclient.Lease, error)
	Renew(ctx context.Context, adapterID, token string, duration time.Duration) (fabricclient.Lease, error)
	RegisterSessionAddress(ctx context.Context, request fabricclient.SessionAddressRequest) (fabricclient.Binding, error)
	Claim(ctx context.Context, request fabricclient.ClaimRequest) (fabricclient.Claim, error)
	Begin(ctx context.Context, deliveryID string, request fabricclient.DispatchRequest) (fabricclient.Delivery, error)
	Release(ctx context.Context, deliveryID string, request fabricclient.ClaimTokenRequest) (fabricclient.Delivery, error)
	Acknowledge(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error)
	Unknown(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error)
	Fail(ctx context.Context, deliveryID string, request fabricclient.ReconcileRequest) (fabricclient.Delivery, error)
	NotInserted(ctx context.Context, deliveryID string, request fabricclient.NotInsertedRequest) (fabricclient.Delivery, error)
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
}

// target is one session address and the DSH session behind it.
type target struct {
	Address    string
	SessionID  string
	Generation int64
	Cwd        string
	UpdatedAt  time.Time
	// State is what the last session/list said: busy, idle or not loaded.
	State string
}

// Driver delivers fabric messages into DSH sessions through the web host.
type Driver struct {
	// Lookup, when set, names a message's sender session; see crewcli.Lookup.
	Lookup func(ctx context.Context, address string) (crewcli.SessionInfo, bool)

	fabric Fabric
	native Native
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	cfg    Config
	log    *slog.Logger

	mu       sync.Mutex
	lease    fabricclient.Lease
	targets  map[string]*target // session id -> target
	parents  map[string]string  // subagent session id -> parent session id
	backoff  map[string]backoff // session id -> when a refused session is tried again
	passes   int
	pumpMu   sync.Mutex
	wakeChan chan struct{}
}

type backoff struct {
	until time.Time
	wait  time.Duration
}

// coldEvery is how many passes a session without a live agent waits between
// claims: waking one is rare, and each claim is a fabric round trip.
const coldEvery = 5

// eventWindow is how many recent user/assistant messages the reconcile check
// reads; the prompt it looks for was made seconds ago.
const eventWindow = 40

func NewDriver(fabric Fabric, native Native, now func() time.Time, cfg Config, log *slog.Logger) *Driver {
	return &Driver{fabric: fabric, native: native, now: now, sleep: sleepContext, cfg: cfg, log: log, targets: map[string]*target{}, parents: map[string]string{}, backoff: map[string]backoff{}, wakeChan: make(chan struct{}, 1)}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	case d.wakeChan <- struct{}{}:
	default:
	}
}

// Run renews the lease and delivers until ctx ends.
func (d *Driver) Run(ctx context.Context, interval time.Duration) error {
	renew := time.NewTicker(d.cfg.LeaseDuration / 3)
	defer renew.Stop()
	pump := time.NewTicker(interval)
	defer pump.Stop()
	failing := false
	for {
		if err := d.Pump(ctx); err != nil && ctx.Err() == nil {
			if !failing {
				d.log.Warn("DSH web host unavailable", "error", err)
			}
			failing = true
		} else if failing {
			d.log.Info("DSH web host reachable again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-renew.C:
			if err := d.renew(ctx); err != nil {
				d.log.Warn("lease renewal failed", "error", err)
			}
		case <-pump.C:
		case <-d.wakeChan:
		}
	}
}

// eligible reports whether a listed session should have an address: a root
// session with a working directory and at least one message, updated recently.
func (d *Driver) eligible(s SessionSummary) bool {
	if s.Origin == "subagent" || s.ParentSessionID != "" || s.Blank || s.Cwd == "" || s.SessionID == "" {
		return false
	}
	return d.now().Sub(time.UnixMilli(s.UpdatedAt)) <= d.cfg.Recent
}

func stateOf(s SessionSummary) string {
	switch {
	case !s.AgentAvailable:
		return "not loaded"
	case s.Running:
		return "busy"
	}
	return "idle"
}

func availabilityOf(state string) string {
	return map[string]string{"busy": "busy", "idle": "idle", "not loaded": "inactive"}[state]
}

// Pump makes one pass: list the sessions, give new ones addresses, then
// deliver each target's eligible work in order. It returns an error only when
// the web host could not be listed.
func (d *Driver) Pump(ctx context.Context) error {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	d.mu.Lock()
	d.passes++
	pass := d.passes
	d.mu.Unlock()
	sessions, err := d.native.List(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	d.refresh(ctx, sessions)
	for _, t := range d.snapshot() {
		if t.State == "not loaded" && pass%coldEvery != 1 {
			continue
		}
		if d.backingOff(t.SessionID) {
			continue
		}
		availability := availabilityOf(t.State)
		for i := 0; i < 20; i++ {
			delivered, err := d.deliverOne(ctx, t, availability)
			if err != nil {
				d.log.Warn("delivery failed", "session", t.SessionID, "address", t.Address, "error", err)
			}
			if !delivered {
				break
			}
			// What was just delivered may have started a turn or loaded the
			// session.
			state, err := d.native.State(ctx, t.SessionID)
			if err != nil {
				break
			}
			availability = availabilityOf(liveState(state))
		}
	}
	return nil
}

func liveState(s SessionState) string {
	switch {
	case !s.Active:
		return "not loaded"
	case s.Running:
		return "busy"
	}
	return "idle"
}

// refresh registers new sessions, records what the list says about known
// ones, and forgets sessions the host no longer lists.
func (d *Driver) refresh(ctx context.Context, sessions []SessionSummary) {
	listed := map[string]bool{}
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			d.mu.Lock()
			d.parents[s.SessionID] = s.ParentSessionID
			d.mu.Unlock()
		}
		listed[s.SessionID] = true
		d.mu.Lock()
		t, known := d.targets[s.SessionID]
		if known {
			t.State, t.Cwd, t.UpdatedAt = stateOf(s), s.Cwd, time.UnixMilli(s.UpdatedAt).UTC()
		}
		d.mu.Unlock()
		// A session that got its address some other way (crew-dsh address or
		// send from a blank session) keeps it while the host lists it.
		if known || !d.eligible(s) {
			continue
		}
		if _, err := d.register(ctx, s); err != nil {
			d.log.Warn("register session failed", "session", s.SessionID, "error", err)
		}
	}
	d.mu.Lock()
	for id := range d.targets {
		if !listed[id] {
			delete(d.targets, id)
		}
	}
	d.mu.Unlock()
}

func (d *Driver) register(ctx context.Context, s SessionSummary) (string, error) {
	binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: s.SessionID, Capabilities: Capabilities})
	if err != nil {
		return "", fmt.Errorf("register session address: %w", err)
	}
	d.mu.Lock()
	d.targets[s.SessionID] = &target{Address: binding.Address, SessionID: s.SessionID, Generation: binding.Generation, Cwd: s.Cwd, UpdatedAt: time.UnixMilli(s.UpdatedAt).UTC(), State: stateOf(s)}
	d.mu.Unlock()
	d.log.Info("registered session", "session", s.SessionID, "address", binding.Address, "cwd", s.Cwd)
	return binding.Address, nil
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

func (d *Driver) backingOff(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.backoff[sessionID]
	return ok && d.now().Before(b.until)
}

// refused holds a session back after the host refused a prompt to it, 30s
// doubling to 10m, so a refusing session does not take a claim every pass.
func (d *Driver) refused(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.backoff[sessionID]
	b.wait = min(max(2*b.wait, 30*time.Second), 10*time.Minute)
	b.until = d.now().Add(b.wait)
	d.backoff[sessionID] = b
}

func (d *Driver) accepted(sessionID string) {
	d.mu.Lock()
	delete(d.backoff, sessionID)
	d.mu.Unlock()
}

// Sessions lists the sessions this driver delivers to, with what the last
// pass read about them.
func (d *Driver) Sessions() []crewcli.SessionInfo {
	out := []crewcli.SessionInfo{}
	for _, t := range d.snapshot() {
		out = append(out, crewcli.SessionInfo{Address: t.Address, Runtime: "dsh", NativeID: t.SessionID, Cwd: t.Cwd, State: t.State, SeenAt: t.UpdatedAt})
	}
	return out
}

// Address returns a session's crew address. A subagent session answers with
// its parent's address, since a subagent cannot take crew messages itself.
func (d *Driver) Address(ctx context.Context, sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", errors.New("session id is required")
	}
	d.mu.Lock()
	if parent, ok := d.parents[sessionID]; ok {
		sessionID = parent
	}
	t, ok := d.targets[sessionID]
	d.mu.Unlock()
	if ok {
		return t.Address, nil
	}
	// A session the driver has not listed yet, such as one created since the
	// last pass.
	sessions, err := d.native.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	byID := map[string]SessionSummary{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	s, ok := byID[sessionID]
	for depth := 0; ok && s.ParentSessionID != "" && depth < 8; depth++ {
		s, ok = byID[s.ParentSessionID]
	}
	if !ok || s.Cwd == "" || s.Origin == "subagent" {
		return "", fmt.Errorf("DSH session %s is not a root session the web host lists", sessionID)
	}
	return d.register(ctx, s)
}

// Send submits a message from a session's own crew address. The policy
// defaults to next_turn; operationID makes retries of one send idempotent.
func (d *Driver) Send(ctx context.Context, request crewcli.SendRequest) (crewcli.SendResult, error) {
	if request.To == "" || strings.TrimSpace(request.Body) == "" || request.OperationID == "" {
		return crewcli.SendResult{}, errors.New("to, body and operation_id are required")
	}
	from, err := d.Address(ctx, request.From)
	if err != nil {
		return crewcli.SendResult{}, fmt.Errorf("session %s has no crew address: %w", request.From, err)
	}
	submitted, err := d.fabric.Submit(ctx, fabricclient.MessageRequest{ProducerID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), OperationID: request.OperationID, SenderAddress: from, RecipientAddress: request.To, Body: request.Body, ActivationPolicy: request.Policy, TTL: "24h"})
	if err != nil {
		return crewcli.SendResult{}, err
	}
	return crewcli.SendResult{MessageID: submitted.Message.MessageID, From: from, To: request.To, Replayed: submitted.Replayed}, nil
}

const attemptPrefix = "dsh:"

// RequestID is the session/prompt requestId of a delivery. The host records
// it as the user message's source.rpcId and deduplicates on it.
func RequestID(deliveryID string) string { return "crew-delivery:" + deliveryID }

func opID(kind, subject string) string { return "crew-dsh/" + kind + "/" + subject }

// attemptSubject names one claim attempt of a delivery.
func attemptSubject(deliveryID string, attempt int) string {
	if attempt <= 1 {
		return deliveryID
	}
	return fmt.Sprintf("%s/%d", deliveryID, attempt)
}

// modeFor maps the sender's policy to a prompt mode: next_step steers (seen
// at the next step boundary, waking an idle agent), next_turn queues (the
// next turn). no_wake never goes through session/prompt; see deliverOne.
func modeFor(policy string) string {
	if policy == "next_step" {
		return "steer"
	}
	return "queue"
}

// claimed is one claimed delivery on its way into a session.
type claimed struct {
	t        target
	claim    fabricclient.Claim
	delivery *fabricclient.Delivery
	action   string
	noWake   bool
	token    string
	ref      string
	attempt  string
}

func (d *Driver) reconcileRequest(c claimed, op string) fabricclient.ReconcileRequest {
	return fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: c.token, OperationID: opID(op, c.delivery.DeliveryID), NativeAttemptRef: c.ref}
}

// deliverOne claims and delivers at most one message for t. It reports
// whether a message went in.
func (d *Driver) deliverOne(ctx context.Context, t target, availability string) (bool, error) {
	token := d.leaseToken()
	now := d.now().UTC()
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: fmt.Sprintf("crew-dsh/claim/%s/%d", t.Address, now.UnixNano()), RecipientAddress: t.Address, RecipientGeneration: t.Generation, Availability: availability, ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		if claim.Reason == "head_dispatching" && claim.Head != nil {
			return false, d.reconcileStuck(ctx, t, *claim.Head)
		}
		return false, nil
	}
	c := claimed{t: t, claim: claim, delivery: claim.Delivery, action: claim.DispatchAction, noWake: claim.Message.ActivationPolicy == "no_wake" || claim.Message.ActivationPolicy == "never_wake", token: token, ref: attemptPrefix + claim.Delivery.DeliveryID}
	if c.action == "" {
		c.action = c.delivery.DispatchAction
	}
	c.attempt = attemptSubject(c.delivery.DeliveryID, c.delivery.AttemptCount)
	release := func(why string) error {
		_, err := d.fabric.Release(ctx, c.delivery.DeliveryID, fabricclient.ClaimTokenRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: fmt.Sprintf("crew-dsh/release/%s/%d", c.delivery.DeliveryID, now.UnixNano()), ClaimToken: claim.ClaimToken})
		if err != nil {
			return fmt.Errorf("release (%s): %w", why, err)
		}
		return nil
	}
	// Read the session before recording the dispatch: if the host is gone,
	// the claim goes back to the queue instead of becoming unknown.
	if _, err := d.native.State(ctx, t.SessionID); err != nil {
		if releaseErr := release("session state unreadable"); releaseErr != nil {
			return false, releaseErr
		}
		return false, fmt.Errorf("read session state: %w", err)
	}
	if _, err := d.fabric.Begin(ctx, c.delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", c.attempt), ClaimToken: claim.ClaimToken, NativeAttemptRef: c.ref}); err != nil {
		return false, fmt.Errorf("begin dispatch: %w", err)
	}
	prompt := PromptRequest{SessionID: t.SessionID, RequestID: RequestID(c.delivery.DeliveryID), Mode: modeFor(claim.Message.ActivationPolicy), Text: d.render(ctx, *claim.Message)}
	return d.dispatch(ctx, c, prompt)
}

// dispatch makes the prompt and settles the delivery from what the host says
// or, when its answer is ambiguous, from what the session holds. A prompt is
// sent again only after the session was checked, after the host's
// inbox-to-history window, and found not to hold it.
//
// A no_wake message goes in with Inject (agent.inject), never with
// session/prompt: a steer wakes an idle agent, and the turn the fabric saw
// running can end between any state read and the steer's admission. Inject
// never starts a turn, so there is no window to check for: a running turn
// takes the message at its next step, and an agent whose turn just ended
// keeps it for the first step of its next turn. It is never steered instead.
func (d *Driver) dispatch(ctx context.Context, c claimed, prompt PromptRequest) (bool, error) {
	send := d.native.Prompt
	if c.noWake {
		send = d.native.Inject
	}
	for sends := 0; ; {
		err := send(ctx, prompt)
		sends++
		if err == nil {
			return true, d.acknowledge(ctx, c, "accepted")
		}
		if ErrorCode(err) == InjectMissing {
			// The web host has no dsh-crew-inject route, so nothing went in.
			// The message goes back as the address head; it is never steered.
			d.log.Warn("no_wake message not delivered: the dsh-crew-inject plugin is not installed in the DSH web host", "session", c.t.SessionID, "delivery", c.delivery.DeliveryID)
			d.refused(c.t.SessionID)
			return false, d.notInserted(ctx, c, "inject_unavailable", err)
		}
		// Wait out the window in which the host has taken the item from the
		// inbox but not yet written the user message, then look for it.
		if sleepErr := d.sleep(ctx, d.cfg.ReconcileWait); sleepErr != nil {
			return false, d.unknown(ctx, c, err)
		}
		has, checkErr := d.holds(ctx, c.t.SessionID, prompt.RequestID)
		switch {
		case checkErr != nil:
			d.log.Warn("prompt outcome unclear and the session could not be checked", "session", c.t.SessionID, "delivery", c.delivery.DeliveryID, "error", err, "check", checkErr)
			return false, d.unknown(ctx, c, err)
		case has:
			return true, d.acknowledge(ctx, c, "reconciled")
		case ErrorCode(err) == "session/not-found":
			// The session is gone; its mail cannot be delivered.
			if _, failErr := d.fabric.Fail(ctx, c.delivery.DeliveryID, d.reconcileRequest(c, "fail")); failErr != nil {
				return false, fmt.Errorf("fail after %v: %w", err, failErr)
			}
			return false, err
		case Definite(err):
			// The host refused the prompt (or never got it), and the session
			// does not hold it.
			d.refused(c.t.SessionID)
			return false, d.notInserted(ctx, c, "prompt_refused", err)
		case Timeout(err) || sends >= 2:
			// The host may still be working on a prompt that timed out, so it
			// is never sent again; a second unclear answer is not retried
			// either.
			return false, d.unknown(ctx, c, err)
		}
		d.log.Info("prompt outcome unclear; the session does not hold it, sending it again", "session", c.t.SessionID, "delivery", c.delivery.DeliveryID, "error", err)
	}
}

func (d *Driver) acknowledge(ctx context.Context, c claimed, how string) error {
	if _, err := d.fabric.Acknowledge(ctx, c.delivery.DeliveryID, d.reconcileRequest(c, "ack")); err != nil {
		return fmt.Errorf("acknowledge: %w", err)
	}
	d.accepted(c.t.SessionID)
	d.log.Info("delivered", "session", c.t.SessionID, "address", c.t.Address, "delivery", c.delivery.DeliveryID, "action", c.action, "native", how)
	return nil
}

func (d *Driver) unknown(ctx context.Context, c claimed, cause error) error {
	if _, err := d.fabric.Unknown(ctx, c.delivery.DeliveryID, d.reconcileRequest(c, "unknown")); err != nil {
		return fmt.Errorf("record outcome unknown after %v: %w", cause, err)
	}
	return fmt.Errorf("delivery %s outcome unknown: %w", c.delivery.DeliveryID, cause)
}

func (d *Driver) notInserted(ctx context.Context, c claimed, reason string, cause error) error {
	if _, err := d.fabric.NotInserted(ctx, c.delivery.DeliveryID, fabricclient.NotInsertedRequest{AdapterID: d.cfg.AdapterID, LeaseToken: c.token, OperationID: opID("not-inserted", c.attempt), ClaimToken: c.claim.ClaimToken, NativeAttemptRef: c.ref, Reason: reason}); err != nil {
		return fmt.Errorf("requeue (%s): %w", reason, err)
	}
	d.log.Info("message not inserted; left pending as the address head", "session", c.t.SessionID, "address", c.t.Address, "delivery", c.delivery.DeliveryID, "reason", reason, "cause", cause)
	return nil
}

// holds reports whether the session holds the prompt with requestID: in its
// inbox, admitted to it, or in the conversation.
func (d *Driver) holds(ctx context.Context, sessionID, requestID string) (bool, error) {
	state, err := d.native.State(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if HasRequest(state, nil, requestID) {
		return true, nil
	}
	events, err := d.native.Events(ctx, sessionID, eventWindow)
	if err != nil {
		return false, err
	}
	return HasRequest(SessionState{}, events, requestID), nil
}

// reconcileStuck settles this driver's own dispatch that a crash left in
// flight, from what the session holds. It is never sent again.
func (d *Driver) reconcileStuck(ctx context.Context, t target, head fabricclient.Delivery) error {
	if head.ClaimOwnerAdapterID != "" && head.ClaimOwnerAdapterID != d.cfg.AdapterID {
		return nil
	}
	if !strings.HasPrefix(head.NativeAttemptRef, attemptPrefix) {
		return nil
	}
	if err := d.sleep(ctx, d.cfg.ReconcileWait); err != nil {
		return err
	}
	has, err := d.holds(ctx, t.SessionID, RequestID(head.DeliveryID))
	if err != nil {
		return fmt.Errorf("reconcile %s: %w", head.DeliveryID, err)
	}
	request := fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeAttemptRef: head.NativeAttemptRef}
	if has {
		request.OperationID = opID("ack", head.DeliveryID)
		_, err = d.fabric.Acknowledge(ctx, head.DeliveryID, request)
	} else {
		request.OperationID = opID("unknown", head.DeliveryID)
		_, err = d.fabric.Unknown(ctx, head.DeliveryID, request)
	}
	if err == nil {
		d.log.Info("reconciled in-flight delivery", "session", t.SessionID, "delivery", head.DeliveryID, "accepted", has)
	}
	return err
}

// render is the text the session reads, naming the sender when Lookup knows it.
func (d *Driver) render(ctx context.Context, m fabricclient.Message) string {
	var sender *crewcli.SessionInfo
	if d.Lookup != nil {
		if info, ok := d.Lookup(ctx, m.SenderAddress); ok {
			sender = &info
		}
	}
	return crewcli.RenderMessage(m.SenderAddress, m.MessageID, m.Body, "crew-dsh", sender)
}
