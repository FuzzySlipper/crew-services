package claudedriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// Capabilities this driver advertises on every session binding. It cannot
// start a Claude Code process, so it never claims wake_inactive. It does not
// claim durable_next_turn either: Claude Code's own follow-up queue is in
// memory, so next_turn work for a busy session stays in the fabric until the
// Stop hook reports the session idle.
var Capabilities = []string{"deliver_next_step", "deliver_when_idle"}

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
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
}

type Config struct {
	AdapterID     string
	InstanceID    string
	StatePath     string
	LeaseDuration time.Duration
	ClaimDuration time.Duration
	// ProjectsDir is Claude Code's transcript root (~/.claude/projects), used
	// when a session registered without a transcript path.
	ProjectsDir string
	// AcceptTimeout bounds how long a posted message may go without the
	// transcript showing the session took it into its queue before the
	// delivery becomes outcome_unknown.
	AcceptTimeout time.Duration
	// QueuedTimeout bounds how long a message the session queued may wait to
	// enter the conversation (for example behind a long tool call).
	QueuedTimeout time.Duration
}

// Session is the driver-private record for one Claude Code session. The
// socket path and token change whenever the process restarts, so they live
// here rather than in the fabric binding.
type Session struct {
	SessionID  string    `json:"session_id"`
	Socket     string    `json:"socket"`
	Token      string    `json:"token,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
	Address    string    `json:"address"`
	Generation int64     `json:"generation"`
	Busy       bool      `json:"busy"`
	SeenAt     time.Time `json:"seen_at"`
}

// Registration is what a SessionStart hook reports about a session.
type Registration struct {
	SessionID  string
	Socket     string
	Token      string
	Cwd        string
	Transcript string
}

// Dispatch is a message posted to a session's inbox and not yet settled. The
// socket accepting the frame says nothing about whether the session took it,
// so the delivery stays dispatching until the session's transcript shows the
// message in the conversation.
type Dispatch struct {
	DeliveryID string `json:"delivery_id"`
	MessageID  string `json:"message_id"`
	MsgID      string `json:"msg_id"`
	Ref        string `json:"native_attempt_ref"`
	SessionID  string `json:"session_id"`
	Socket     string `json:"socket"`
	Transcript string `json:"transcript"`
	Offset     int64  `json:"offset"`
	Enqueued   bool   `json:"enqueued,omitempty"`
	// Inserted records that the transcript showed the message in the
	// conversation. It is saved before the acknowledgement is sent, so a failed
	// acknowledgement is retried (with the same operation ID) instead of being
	// lost behind an offset that already moved past the evidence.
	Inserted bool      `json:"inserted,omitempty"`
	PostedAt time.Time `json:"posted_at"`
}

type Driver struct {
	// Lookup, when set, names a message's sender session; see crewcli.Lookup.
	Lookup func(ctx context.Context, address string) (crewcli.SessionInfo, bool)

	fabric Fabric
	inbox  Inbox
	now    func() time.Time
	cfg    Config
	log    *slog.Logger

	mu       sync.Mutex
	lease    fabricclient.Lease
	sessions map[string]*Session
	// dispatches are keyed by delivery ID.
	dispatches map[string]*Dispatch
	pumpMu     sync.Mutex
	wake       chan struct{}
}

func New(fabric Fabric, inbox Inbox, now func() time.Time, cfg Config, log *slog.Logger) (*Driver, error) {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.ClaimDuration == 0 {
		cfg.ClaimDuration = time.Minute
	}
	if cfg.AcceptTimeout == 0 {
		cfg.AcceptTimeout = 2 * time.Minute
	}
	if cfg.QueuedTimeout == 0 {
		cfg.QueuedTimeout = 6 * time.Hour
	}
	d := &Driver{fabric: fabric, inbox: inbox, now: now, cfg: cfg, log: log, sessions: map[string]*Session{}, dispatches: map[string]*Dispatch{}, wake: make(chan struct{}, 1)}
	if err := d.load(); err != nil {
		return nil, err
	}
	return d, nil
}

// Start takes the adapter lease. Re-registering after a restart replaces the
// previous instance's lease.
func (d *Driver) Start(ctx context.Context) error {
	lease, err := d.fabric.Register(ctx, d.cfg.AdapterID, d.cfg.InstanceID, d.cfg.LeaseDuration)
	if err != nil {
		return fmt.Errorf("register adapter: %w", err)
	}
	d.mu.Lock()
	d.lease = lease
	d.mu.Unlock()
	d.refreshCapabilities(ctx)
	return nil
}

// refreshCapabilities re-registers every known session so its binding carries
// the current Capabilities (an older build claimed durable_next_turn).
// Registration is idempotent and keeps the address and generation.
func (d *Driver) refreshCapabilities(ctx context.Context) {
	for _, s := range d.snapshot() {
		binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: s.SessionID, Capabilities: Capabilities})
		if err != nil {
			d.log.Warn("refresh session capabilities", "session", s.SessionID, "error", err)
			continue
		}
		d.mu.Lock()
		if current, ok := d.sessions[s.SessionID]; ok {
			current.Address, current.Generation = binding.Address, binding.Generation
		}
		d.mu.Unlock()
	}
}

func (d *Driver) leaseToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease.LeaseToken
}

// RegisterSession records a session's inbox and returns its crew address.
func (d *Driver) RegisterSession(ctx context.Context, r Registration) (string, error) {
	if r.SessionID == "" || r.Socket == "" {
		return "", errors.New("session_id and socket are required")
	}
	binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: r.SessionID, Capabilities: Capabilities})
	if err != nil {
		return "", fmt.Errorf("register session address: %w", err)
	}
	d.mu.Lock()
	d.sessions[r.SessionID] = &Session{SessionID: r.SessionID, Socket: r.Socket, Token: r.Token, Cwd: r.Cwd, Transcript: r.Transcript, Address: binding.Address, Generation: binding.Generation, SeenAt: d.now().UTC()}
	err = d.saveLocked()
	d.mu.Unlock()
	d.Poke()
	return binding.Address, err
}

// SetBusy records the session's turn state from its hooks. Unknown sessions
// are ignored; SessionStart registers them.
func (d *Driver) SetBusy(sessionID string, busy bool) {
	d.mu.Lock()
	s, ok := d.sessions[sessionID]
	if ok {
		s.Busy = busy
		s.SeenAt = d.now().UTC()
		_ = d.saveLocked()
	}
	d.mu.Unlock()
	if ok && !busy {
		d.Poke()
	}
}

// Address returns the crew address of a registered session.
func (d *Driver) Address(sessionID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[sessionID]
	if !ok {
		return "", false
	}
	return s.Address, true
}

// Sessions lists the sessions this driver has registered.
func (d *Driver) Sessions() []crewcli.SessionInfo {
	out := []crewcli.SessionInfo{}
	for _, s := range d.snapshot() {
		state := d.availability(s)
		if state == "inactive" {
			state = "exited"
		}
		out = append(out, crewcli.SessionInfo{Address: s.Address, Runtime: "claude-code", NativeID: s.SessionID, Cwd: s.Cwd, State: state, SeenAt: s.SeenAt})
	}
	return out
}

// Send submits a message from a registered session's own address. The policy
// defaults to next_turn; operationID makes retries of one send idempotent.
func (d *Driver) Send(ctx context.Context, request crewcli.SendRequest) (crewcli.SendResult, error) {
	from, ok := d.Address(request.From)
	if !ok {
		return crewcli.SendResult{}, fmt.Errorf("session %s is not registered with crew-claude; it registers when it starts or resumes", request.From)
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

// Poke asks the run loop for an immediate delivery pass.
func (d *Driver) Poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run renews the lease and delivers pending work until ctx ends.
func (d *Driver) Run(ctx context.Context, interval time.Duration) error {
	renew := time.NewTicker(d.cfg.LeaseDuration / 3)
	defer renew.Stop()
	pump := time.NewTicker(interval)
	defer pump.Stop()
	for {
		d.Pump(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-renew.C:
			if err := d.renew(ctx); err != nil {
				d.log.Warn("lease renewal failed", "error", err)
			}
		case <-pump.C:
		case <-d.wake:
		}
	}
}

func (d *Driver) renew(ctx context.Context) error {
	lease, err := d.fabric.Renew(ctx, d.cfg.AdapterID, d.leaseToken(), d.cfg.LeaseDuration)
	if err != nil {
		// A lost lease (expired or fenced) is taken again; queued claims of the
		// old instance return to the queue on their own.
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

// Pump makes one delivery pass over every registered session, draining each
// session's eligible work in order. Each delivery reads the session's turn
// state afresh, since a hook can change it in the middle of a pass.
func (d *Driver) Pump(ctx context.Context) {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	for _, s := range d.snapshot() {
		for i := 0; i < 20; i++ {
			delivered, err := d.deliverOne(ctx, s.SessionID)
			if err != nil {
				d.log.Warn("delivery pass failed", "session", s.SessionID, "address", s.Address, "error", err)
			}
			if !delivered {
				break
			}
		}
	}
}

func (d *Driver) snapshot() []Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// session returns the current record of one session. A session that
// registered without a transcript path (or before the driver kept one) gets
// Claude Code's default location.
func (d *Driver) session(sessionID string) (Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[sessionID]
	if !ok {
		return Session{}, false
	}
	out := *s
	if out.Transcript == "" {
		out.Transcript = defaultTranscript(d.cfg.ProjectsDir, out.Cwd, out.SessionID)
	}
	return out, true
}

func (d *Driver) availability(s Session) string {
	if _, err := os.Stat(s.Socket); err != nil {
		return "inactive"
	}
	if s.Busy {
		return "busy"
	}
	return "idle"
}

// deliverOne claims and posts at most one message for a session, or settles
// the one it posted earlier. It reports whether a message went out, so the
// caller keeps draining.
func (d *Driver) deliverOne(ctx context.Context, sessionID string) (bool, error) {
	s, ok := d.session(sessionID)
	if !ok {
		return false, nil
	}
	token := d.leaseToken()
	now := d.now().UTC()
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("claim", s.Address, now), RecipientAddress: s.Address, RecipientGeneration: s.Generation, Availability: d.availability(s), ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		if claim.Reason == "head_dispatching" && claim.Head != nil {
			// Settling the head lets the next message through in this pass.
			return d.reconcile(ctx, s, *claim.Head)
		}
		return false, nil
	}
	delivery := claim.Delivery
	action := claim.DispatchAction
	if action == "" {
		action = delivery.DispatchAction
	}
	release := func(cause error) error {
		if _, err := d.fabric.Release(ctx, delivery.DeliveryID, fabricclient.ClaimTokenRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("release", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken}); err != nil {
			return fmt.Errorf("release after %v: %w", cause, err)
		}
		return nil
	}
	// Open the socket before recording the dispatch: a session that is gone
	// sends the claim back to the queue instead of leaving it unknown.
	conn, err := d.inbox.Dial(ctx, s.Socket, s.Token)
	if err != nil {
		return false, release(err)
	}
	defer conn.Close()
	// Read the turn state again just before the native side effect: a Stop
	// or UserPromptSubmit hook may have arrived since the claim.
	priority, stale := d.priorityFor(action, claim.Message.ActivationPolicy, sessionID)
	if stale != "" {
		d.log.Info("turn state changed before posting; message stays queued", "session", s.SessionID, "delivery", delivery.DeliveryID, "reason", stale)
		return false, release(errors.New(stale))
	}
	ref := delivery.DeliveryID
	if _, err := d.fabric.Begin(ctx, delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref}); err != nil {
		return false, fmt.Errorf("begin dispatch: %w", err)
	}
	pending := &Dispatch{DeliveryID: delivery.DeliveryID, MessageID: claim.Message.MessageID, MsgID: nativeMessageID(delivery.DeliveryID), Ref: ref, SessionID: s.SessionID, Socket: s.Socket, Transcript: s.Transcript, Offset: fileSize(s.Transcript), PostedAt: now}
	d.mu.Lock()
	d.dispatches[pending.DeliveryID] = pending
	saveErr := d.saveLocked()
	d.mu.Unlock()
	if saveErr != nil {
		d.log.Warn("save driver state", "error", saveErr)
	}
	// Whether or not the write succeeds, the delivery stays dispatching until
	// the transcript settles it: a write that failed may still have arrived,
	// and one that succeeded may still be held or refused by the session.
	if err := conn.Post(pending.MsgID, d.render(ctx, *claim.Message), priority); err != nil {
		d.log.Warn("inbox write failed; settling from the transcript", "session", s.SessionID, "delivery", delivery.DeliveryID, "error", err)
		return false, nil
	}
	d.log.Info("posted", "session", s.SessionID, "address", s.Address, "delivery", delivery.DeliveryID, "action", action, "priority", priority)
	return true, nil
}

// priorityFor picks the inbox priority for a claimed delivery from the
// session's current turn state, or says why the message must wait instead.
//
// "next" is read at the active turn's next step boundary, but it also starts a
// turn on an idle session, and Claude Code has no frame that enters only an
// active turn. So a no_wake message goes out only while the session is busy,
// and a next_turn message only while it is idle (a "later" frame to a busy
// session would sit in the session's in-memory queue, which an exit loses).
func (d *Driver) priorityFor(action, policy, sessionID string) (string, string) {
	s, ok := d.session(sessionID)
	if !ok {
		return "", "session no longer registered"
	}
	policy, _ = normalizePolicy(policy)
	switch action {
	case "deliver_next_step":
		if !s.Busy && policy == "no_wake" {
			return "", "no_wake target went idle"
		}
		return PriorityNext, ""
	case "deliver_at_idle":
		if s.Busy {
			if policy == "next_step" {
				return PriorityNext, ""
			}
			return "", "next_turn target became busy"
		}
		return PriorityLater, ""
	}
	return "", "unsupported dispatch action " + action
}

func normalizePolicy(policy string) (string, bool) {
	switch policy {
	case "", "next_turn", "wake_when_idle":
		return "next_turn", true
	case "next_step":
		return "next_step", true
	case "no_wake", "never_wake":
		return "no_wake", true
	}
	return policy, false
}

// reconcile settles this driver's dispatching head from the session's
// transcript: delivered once the message is in the conversation,
// outcome_unknown when the session never queued it within AcceptTimeout, or
// queued it and then lost it (the process exited) or kept it past
// QueuedTimeout. It never posts again, and reports whether it settled the
// delivery.
func (d *Driver) reconcile(ctx context.Context, s Session, head fabricclient.Delivery) (bool, error) {
	if head.ClaimOwnerAdapterID != "" && head.ClaimOwnerAdapterID != d.cfg.AdapterID {
		return false, nil
	}
	now := d.now().UTC()
	d.mu.Lock()
	pending, ok := d.dispatches[head.DeliveryID]
	if !ok {
		// Posted by an instance that lost its state, or crashed between Begin
		// and saving: look for it from the start of the transcript.
		pending = &Dispatch{DeliveryID: head.DeliveryID, MessageID: head.MessageID, MsgID: nativeMessageID(head.DeliveryID), Ref: head.NativeAttemptRef, SessionID: s.SessionID, Socket: s.Socket, Transcript: s.Transcript, PostedAt: now}
		d.dispatches[head.DeliveryID] = pending
	}
	p := *pending
	d.mu.Unlock()
	if p.Transcript == "" {
		p.Transcript = s.Transcript
	}
	if head.NativeAttemptRef != "" {
		p.Ref = head.NativeAttemptRef
	}
	if !p.Inserted {
		marker := ""
		if p.MessageID != "" {
			marker = "(crew message " + p.MessageID + ")"
		}
		scan, err := scanTranscript(p.Transcript, p.Offset, p.MsgID, marker)
		if err != nil {
			return false, fmt.Errorf("read transcript %s: %w", p.Transcript, err)
		}
		p.Offset = scan.Offset
		p.Enqueued = p.Enqueued || scan.Enqueued
		if scan.Inserted {
			// Keep the evidence before acting on it: the offset now points past
			// the line that proved the insertion.
			p.Inserted = true
			if err := d.keepDispatch(p); err != nil {
				d.log.Warn("save driver state", "error", err)
			}
		}
	}
	age := now.Sub(p.PostedAt)
	request := fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeAttemptRef: p.Ref}
	var outcome string
	var err error
	switch {
	case p.Inserted:
		outcome = "delivered"
		// The same operation ID on every attempt: a retry after a lost
		// response replays the first acknowledgement.
		request.OperationID = "crew-claude/ack/" + p.DeliveryID
		_, err = d.fabric.Acknowledge(ctx, p.DeliveryID, request)
	case !p.Enqueued && age > d.cfg.AcceptTimeout:
		outcome = "outcome_unknown: never queued by the session"
	case p.Enqueued && p.Socket != s.Socket:
		outcome = "outcome_unknown: queued, then the session process exited"
	case p.Enqueued && d.availability(s) == "inactive":
		outcome = "outcome_unknown: queued, then the session process exited"
	case p.Enqueued && age > d.cfg.QueuedTimeout:
		outcome = "outcome_unknown: queued but never entered the conversation"
	}
	if outcome != "" && !p.Inserted {
		request.OperationID = "crew-claude/unknown/" + p.DeliveryID
		_, err = d.fabric.Unknown(ctx, p.DeliveryID, request)
	}
	var gone *fabricclient.HTTPError
	settled := outcome != "" && err == nil
	if outcome != "" && errors.As(err, &gone) && (gone.Status == 404 || gone.Status == 409) {
		// Already settled elsewhere (for example by fabric maintenance).
		settled, err = true, nil
	}
	if !settled {
		if saveErr := d.keepDispatch(p); saveErr != nil && err == nil {
			err = saveErr
		}
		if err != nil && p.Inserted {
			err = fmt.Errorf("acknowledge %s (kept for retry): %w", p.DeliveryID, err)
		}
		return false, err
	}
	d.mu.Lock()
	delete(d.dispatches, p.DeliveryID)
	saveErr := d.saveLocked()
	d.mu.Unlock()
	d.log.Info("settled dispatch", "session", s.SessionID, "delivery", p.DeliveryID, "outcome", outcome, "after", age.Round(time.Millisecond))
	return true, saveErr
}

// keepDispatch records a dispatch's progress in memory and in the state file.
// A dispatch another pass settled in the meantime stays settled.
func (d *Driver) keepDispatch(p Dispatch) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.dispatches[p.DeliveryID]
	if !ok {
		return nil
	}
	*current = p
	return d.saveLocked()
}

// render is the text the session reads, naming the sender when Lookup knows it.
func (d *Driver) render(ctx context.Context, m fabricclient.Message) string {
	var sender *crewcli.SessionInfo
	if d.Lookup != nil {
		if info, ok := d.Lookup(ctx, m.SenderAddress); ok {
			sender = &info
		}
	}
	return crewcli.RenderMessage(m.SenderAddress, m.MessageID, m.Body, "crew-claude", sender)
}

// nativeMessageID derives the UUID-shaped msg_id the inbox records (it ignores
// any other shape) from the fabric delivery ID, so a redelivery of the same
// delivery carries the same native ID.
func nativeMessageID(deliveryID string) string {
	sum := sha256.Sum256([]byte("crew-claude/" + deliveryID))
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var opSeq atomic.Int64

// opID names one fabric operation attempt. The sequence keeps two attempts
// in the same clock tick apart.
func opID(kind, subject string, now time.Time) string {
	return fmt.Sprintf("crew-claude/%s/%s/%d-%d", kind, subject, now.UnixNano(), opSeq.Add(1))
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
		Sessions   []*Session  `json:"sessions"`
		Dispatches []*Dispatch `json:"dispatches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse driver state: %w", err)
	}
	for _, s := range stored.Sessions {
		d.sessions[s.SessionID] = s
	}
	for _, p := range stored.Dispatches {
		d.dispatches[p.DeliveryID] = p
	}
	return nil
}

func (d *Driver) saveLocked() error {
	if d.cfg.StatePath == "" {
		return nil
	}
	list := make([]*Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SessionID < list[j].SessionID })
	pending := make([]*Dispatch, 0, len(d.dispatches))
	for _, p := range d.dispatches {
		pending = append(pending, p)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].DeliveryID < pending[j].DeliveryID })
	data, err := json.MarshalIndent(struct {
		Sessions   []*Session  `json:"sessions"`
		Dispatches []*Dispatch `json:"dispatches"`
	}{list, pending}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.cfg.StatePath), 0o700); err != nil {
		return err
	}
	tmp := d.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.cfg.StatePath)
}
