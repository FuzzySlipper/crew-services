package agydriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// Capabilities this driver advertises on every session binding. It cannot
// start an agy process, so it never claims wake_inactive. It does not claim
// durable_next_turn either: SendAgentMessage has no "after this turn" form
// (a message posted to a running turn is read at its next step), so next_turn
// work for a busy conversation stays in the fabric until the Stop hook, or the
// conversation's run state, says it is idle.
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
	NotInserted(ctx context.Context, deliveryID string, request fabricclient.NotInsertedRequest) (fabricclient.Delivery, error)
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
	ListSeats(ctx context.Context, filter fabricclient.SeatFilter) ([]fabricclient.Seat, error)
	ListSeatHoldings(ctx context.Context, seat string) ([]fabricclient.Holding, error)
	ListDispatching(ctx context.Context, adapterID string) ([]fabricclient.Delivery, error)
	SeatWrite(ctx context.Context, write, seat string, request fabricclient.SeatWriteRequest) (json.RawMessage, error)
}

// Processes is how the driver sees agy processes (Procs reads /proc).
type Processes interface {
	Alive(proc Proc) bool
	ListenPorts(proc Proc) ([]int, error)
	Resuming() map[string]Proc
}

type Config struct {
	AdapterID     string
	InstanceID    string
	StatePath     string
	LeaseDuration time.Duration
	ClaimDuration time.Duration
	// TokenPath holds the CSRF token agy processes are started with
	// (ANTIGRAVITY_SERVER_CSRF_TOKEN). Without it the driver can still hand
	// messages to a running turn through the PreInvocation hook, but cannot
	// wake a conversation.
	TokenPath string
	// AcceptTimeout bounds how long a posted message may go without the
	// conversation's message folder showing it before the delivery becomes
	// outcome_unknown.
	AcceptTimeout time.Duration
	// QueuedTimeout bounds how long a message the conversation took in may
	// wait to enter the conversation (behind a long tool call, say).
	QueuedTimeout time.Duration
}

// Session is the driver-private record for one agy conversation. The process
// and its language server port change whenever agy restarts, so they live
// here rather than in the fabric binding.
type Session struct {
	ConversationID string    `json:"conversation_id"`
	Proc           Proc      `json:"proc"`
	LSAddr         string    `json:"ls_addr,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	Transcript     string    `json:"transcript,omitempty"`
	Address        string    `json:"address"`
	Generation     int64     `json:"generation"`
	Busy           bool      `json:"busy"`
	SeenAt         time.Time `json:"seen_at"`
}

// Registration is what a hook reports about a conversation.
type Registration struct {
	ConversationID string
	Proc           Proc
	Cwd            string
	Transcript     string
}

// Dispatch is a message posted to a conversation and not yet settled. The
// language server's empty answer says nothing about whether the conversation
// took it, so the delivery stays dispatching until the transcript shows it.
type Dispatch struct {
	DeliveryID     string `json:"delivery_id"`
	MessageID      string `json:"message_id"`
	Ref            string `json:"native_attempt_ref"`
	ConversationID string `json:"conversation_id"`
	Proc           Proc   `json:"proc"`
	Transcript     string `json:"transcript"`
	Offset         int64  `json:"offset"`
	// Via is "hook" for a message returned from a PreInvocation hook, and
	// empty for a SendAgentMessage post.
	Via      string `json:"via,omitempty"`
	Enqueued bool   `json:"enqueued,omitempty"`
	// Inserted records that the transcript showed the message. It is saved
	// before the acknowledgement goes out, so a failed acknowledgement is
	// retried with the same operation ID.
	Inserted bool      `json:"inserted,omitempty"`
	PostedAt time.Time `json:"posted_at"`
}

type Driver struct {
	// Lookup, when set, names a message's sender session; see crewcli.Lookup.
	Lookup func(ctx context.Context, address string) (crewcli.SessionInfo, bool)

	fabric Fabric
	native Native
	procs  Processes
	now    func() time.Time
	cfg    Config
	log    *slog.Logger

	mu         sync.Mutex
	lease      fabricclient.Lease
	sessions   map[string]*Session
	dispatches map[string]*Dispatch
	pumpMu     sync.Mutex
	wake       chan struct{}
	passes     int
	// noWakeHeld marks conversations whose fabric head is a no_wake message
	// the pump released, so passes leave it for a PreInvocation hook.
	noWakeHeld map[string]time.Time
	seats      map[string][]fabricclient.SeatRoute
}

const noWakeHoldFor = 30 * time.Second

// turnContextBudget bounds the driver's side of a PreInvocation hook call,
// well inside the hook's own 1.5s.
const turnContextBudget = time.Second

// resumeScanEvery is how many passes go by between looks for agy processes
// resuming a known conversation with --conversation.
const resumeScanEvery = 5

func New(fabric Fabric, native Native, procs Processes, now func() time.Time, cfg Config, log *slog.Logger) (*Driver, error) {
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
	if cfg.AdapterID == "" {
		cfg.AdapterID = "antigravity"
	}
	d := &Driver{fabric: fabric, native: native, procs: procs, now: now, cfg: cfg, log: log, sessions: map[string]*Session{}, dispatches: map[string]*Dispatch{}, wake: make(chan struct{}, 1), noWakeHeld: map[string]time.Time{}, seats: map[string][]fabricclient.SeatRoute{}}
	if err := d.load(); err != nil {
		return nil, err
	}
	return d, nil
}

// token is the CSRF token agy processes are started with, read afresh so a
// new token file applies without a restart.
func (d *Driver) token() string {
	if d.cfg.TokenPath == "" {
		return ""
	}
	data, err := os.ReadFile(d.cfg.TokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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

// Reachable is how a conversation can be reached, as reported to its hooks.
type Reachable struct {
	Address string
	// Wakeable: the driver can post to the conversation's language server,
	// so crew messages can start a turn. Otherwise they reach it only inside
	// turns, through its PreInvocation hook.
	Wakeable bool
	Seats    []string
}

// ErrSubagent refuses a conversation another conversation started: its hooks
// run in the same agy process, but it is not a crew session of its own.
var ErrSubagent = errors.New("subagent conversation")

// RegisterSession records a conversation and returns how it is reached. A
// hook from a conversation the driver already knows in the same process only
// refreshes it (and looks for its language server if it has none yet); a new
// conversation, or a known one in a new process (agy --conversation resumes
// without a SessionStart hook), registers its address.
func (d *Driver) RegisterSession(ctx context.Context, r Registration) (Reachable, error) {
	if r.ConversationID == "" {
		return Reachable{}, errors.New("conversation_id is required")
	}
	d.mu.Lock()
	existing, known := d.sessions[r.ConversationID]
	var current Session
	if known {
		current = *existing
	}
	d.mu.Unlock()
	if known && current.Proc == r.Proc {
		addr := current.LSAddr
		if addr == "" {
			if addr, _ = d.locate(ctx, r.Proc, r.ConversationID); addr != "" {
				d.mu.Lock()
				if s, ok := d.sessions[r.ConversationID]; ok {
					s.LSAddr = addr
				}
				d.mu.Unlock()
			}
		}
		d.touch(r)
		return Reachable{Address: current.Address, Wakeable: addr != "", Seats: d.HeldSeats(current.Address)}, nil
	}
	addr, parent := d.locate(ctx, r.Proc, r.ConversationID)
	if parent != "" {
		return Reachable{}, ErrSubagent
	}
	binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: r.ConversationID, Capabilities: Capabilities})
	if err != nil {
		return Reachable{}, fmt.Errorf("register session address: %w", err)
	}
	d.mu.Lock()
	delete(d.noWakeHeld, r.ConversationID)
	s := &Session{ConversationID: r.ConversationID, Proc: r.Proc, LSAddr: addr, Cwd: r.Cwd, Transcript: r.Transcript, Address: binding.Address, Generation: binding.Generation, SeenAt: d.now().UTC()}
	if known {
		s.Busy = existing.Busy
		if s.Cwd == "" {
			s.Cwd = existing.Cwd
		}
		if s.Transcript == "" {
			s.Transcript = existing.Transcript
		}
	}
	d.sessions[r.ConversationID] = s
	err = d.saveLocked()
	d.mu.Unlock()
	d.refreshSeats(ctx)
	d.Poke()
	return Reachable{Address: binding.Address, Wakeable: addr != "", Seats: d.HeldSeats(binding.Address)}, err
}

// touch records a hook from a known conversation in its current process.
func (d *Driver) touch(r Registration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[r.ConversationID]; ok {
		s.SeenAt = d.now().UTC()
		if r.Transcript != "" {
			s.Transcript = r.Transcript
		}
		if r.Cwd != "" {
			s.Cwd = r.Cwd
		}
		_ = d.saveLocked()
	}
}

// locate finds the language server of proc that answers for conversationID
// with the driver's token, and the conversation's parent if it has one. The
// address is "" when there is none. agy listens on an HTTPS and an HTTP port;
// only the HTTP one answers a plain request.
func (d *Driver) locate(ctx context.Context, proc Proc, conversationID string) (addr, parent string) {
	token := d.token()
	if proc.PID == 0 || token == "" || d.procs == nil {
		return "", ""
	}
	ports, err := d.procs.ListenPorts(proc)
	if err != nil {
		return "", ""
	}
	for _, port := range ports {
		addr := "127.0.0.1:" + strconv.Itoa(port)
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		state, err := d.native.Conversation(callCtx, addr, token, conversationID)
		cancel()
		if err == nil {
			return addr, state.Parent
		}
		var refused *RefusedError
		if errors.As(err, &refused) && refused.Code == "unauthenticated" {
			d.log.Warn("agy was not started with the driver's token; its conversations cannot be woken", "pid", proc.PID, "token_file", d.cfg.TokenPath)
			return "", ""
		}
	}
	return "", ""
}

// SetBusy records the conversation's turn state from its hooks.
func (d *Driver) SetBusy(conversationID string, busy bool) {
	d.mu.Lock()
	s, ok := d.sessions[conversationID]
	if ok {
		delete(d.noWakeHeld, conversationID)
		s.Busy = busy
		s.SeenAt = d.now().UTC()
		_ = d.saveLocked()
	}
	d.mu.Unlock()
	if ok && !busy {
		d.Poke()
	}
}

// Address returns the crew address of a registered conversation.
func (d *Driver) Address(conversationID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[conversationID]
	if !ok {
		return "", false
	}
	return s.Address, true
}

// Model is the model a conversation's newest step used, from its transcript.
func (d *Driver) Model(conversationID string) string {
	s, ok := d.session(conversationID)
	if !ok {
		return ""
	}
	return crewcli.LastModel(s.Transcript, transcriptModel)
}

// Sessions lists the conversations this driver has registered.
func (d *Driver) Sessions() []crewcli.SessionInfo {
	out := []crewcli.SessionInfo{}
	for _, s := range d.snapshot() {
		state := d.availability(s)
		if state == "inactive" {
			state = "exited"
		}
		out = append(out, crewcli.SessionInfo{Address: s.Address, Runtime: "antigravity", NativeID: s.ConversationID, Cwd: s.Cwd, State: state, SeenAt: s.SeenAt})
	}
	return out
}

// Send submits a message from a registered conversation's own address.
func (d *Driver) Send(ctx context.Context, request crewcli.SendRequest) (crewcli.SendResult, error) {
	from, ok := d.Address(request.From)
	if !ok {
		return crewcli.SendResult{}, fmt.Errorf("conversation %s is not registered with crew-agy; it registers at its next turn", request.From)
	}
	if request.To == "" || strings.TrimSpace(request.Body) == "" || request.OperationID == "" {
		return crewcli.SendResult{}, errors.New("to, body and operation_id are required")
	}
	submitted, err := d.fabric.Submit(ctx, fabricclient.MessageRequest{ProducerID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), OperationID: request.OperationID, SenderAddress: from, RecipientAddress: request.To, Body: request.Body, ActivationPolicy: request.Policy, TTL: "24h", AsSeat: request.AsSeat})
	if err != nil {
		return crewcli.SendResult{}, err
	}
	return crewcli.SendResult{MessageID: submitted.Message.MessageID, From: submitted.Message.SenderAddress, To: request.To, Replayed: submitted.Replayed}, nil
}

// SeatWrite makes a seat write for a registered conversation.
func (d *Driver) SeatWrite(ctx context.Context, request crewcli.SeatRequest) (json.RawMessage, error) {
	from, ok := d.Address(request.From)
	if !ok {
		return nil, fmt.Errorf("conversation %s is not registered with crew-agy; it registers at its next turn", request.From)
	}
	body, err := crewcli.SeatWriteRequest(request, d.cfg.AdapterID, d.leaseToken(), from)
	if err != nil {
		return nil, err
	}
	result, err := d.fabric.SeatWrite(ctx, request.Write, request.Seat, body)
	if err == nil {
		d.Poke()
	}
	return result, err
}

// HeldSeats lists the seats an address holds, as the last pass read them.
func (d *Driver) HeldSeats(address string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for _, r := range d.seats[address] {
		out = append(out, r.Seat)
	}
	return out
}

func (d *Driver) refreshSeats(ctx context.Context) {
	seats, err := fabricclient.HeldSeats(ctx, d.fabric, d.cfg.AdapterID)
	if err != nil {
		d.log.Warn("list held seats", "error", err)
		return
	}
	d.mu.Lock()
	d.seats = seats
	d.mu.Unlock()
}

// route is one fabric queue a conversation takes mail from: its own address,
// or a seat it holds.
type route struct {
	Address    string
	Generation int64
}

func (d *Driver) routes(s Session) []route {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []route{{s.Address, s.Generation}}
	for _, r := range d.seats[s.Address] {
		out = append(out, route{r.Seat, r.Generation})
	}
	return out
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

// Pump makes one delivery pass over every registered conversation.
func (d *Driver) Pump(ctx context.Context) {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	d.passes++
	if d.passes%resumeScanEvery == 1 {
		d.adoptResumed(ctx)
	}
	d.refreshSeats(ctx)
	d.settleSeatDispatches(ctx)
	for _, s := range d.snapshot() {
		for _, r := range d.routes(s) {
			for i := 0; i < 20; i++ {
				delivered, err := d.deliverOne(ctx, s.ConversationID, r)
				if err != nil {
					d.log.Warn("delivery pass failed", "conversation", s.ConversationID, "address", r.Address, "error", err)
				}
				if !delivered {
					break
				}
			}
		}
	}
}

// adoptResumed attaches a known conversation whose process exited to an agy
// process now resuming it with --conversation, before that conversation runs
// a turn (agy runs no SessionStart hook for a resumed conversation).
func (d *Driver) adoptResumed(ctx context.Context) {
	if d.procs == nil {
		return
	}
	var stale []Session
	for _, s := range d.snapshot() {
		if !d.procs.Alive(s.Proc) {
			stale = append(stale, s)
		}
	}
	if len(stale) == 0 {
		return
	}
	resuming := d.procs.Resuming()
	for _, s := range stale {
		proc, ok := resuming[s.ConversationID]
		if !ok {
			continue
		}
		addr, _ := d.locate(ctx, proc, s.ConversationID)
		d.mu.Lock()
		if current, ok := d.sessions[s.ConversationID]; ok && current.Proc == s.Proc {
			current.Proc, current.LSAddr, current.Busy = proc, addr, false
			current.SeenAt = d.now().UTC()
			_ = d.saveLocked()
		}
		d.mu.Unlock()
		d.log.Info("conversation resumed in a new agy process", "conversation", s.ConversationID, "pid", proc.PID, "wakeable", addr != "")
	}
}

func (d *Driver) snapshot() []Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConversationID < out[j].ConversationID })
	return out
}

func (d *Driver) session(conversationID string) (Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[conversationID]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

// availability is the conversation's state for a fabric claim: inactive when
// its agy process has exited, or when a newer conversation has taken over that
// process (after /new, say); otherwise busy or idle from its hooks.
func (d *Driver) availability(s Session) string {
	if d.procs == nil || !d.procs.Alive(s.Proc) || d.superseded(s) {
		return "inactive"
	}
	if s.Busy {
		return "busy"
	}
	return "idle"
}

// superseded reports whether another conversation in the same agy process ran
// a hook more recently: the process's terminal now shows that one, and a
// message posted to s would run it unseen in the background.
func (d *Driver) superseded(s Session) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, other := range d.sessions {
		if id != s.ConversationID && other.Proc == s.Proc && other.SeenAt.After(s.SeenAt) {
			return true
		}
	}
	return false
}

// deliverOne claims and posts at most one message from one of a
// conversation's routes, or settles the one it posted earlier.
func (d *Driver) deliverOne(ctx context.Context, conversationID string, r route) (bool, error) {
	s, ok := d.session(conversationID)
	if !ok {
		return false, nil
	}
	token := d.leaseToken()
	now := d.now().UTC()
	d.mu.Lock()
	heldAt, held := d.noWakeHeld[conversationID]
	d.mu.Unlock()
	if held && now.Sub(heldAt) < noWakeHoldFor {
		return false, nil
	}
	availability := d.availability(s)
	if s.LSAddr == "" {
		// Nothing can be posted: the claim only settles a head left
		// dispatching. Messages wait for the conversation's next turn.
		availability = "inactive"
	}
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("claim", r.Address, now), RecipientAddress: r.Address, RecipientGeneration: r.Generation, Availability: availability, ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		if claim.Reason == "head_dispatching" && claim.Head != nil {
			return d.reconcileHead(ctx, s, *claim.Head)
		}
		return false, nil
	}
	delivery := claim.Delivery
	action := claim.DispatchAction
	if action == "" {
		action = delivery.DispatchAction
	}
	release := d.releaser(ctx, token, claim, now)
	if claim.HolderAddress != "" && claim.HolderAddress != s.Address {
		return false, release(errors.New("seat " + r.Address + " is now held by " + claim.HolderAddress))
	}
	policy, _ := normalizePolicy(claim.Message.ActivationPolicy)
	if policy == "no_wake" {
		// A posted message starts a turn on an idle conversation, and a turn
		// can end between reading its state and posting. So no_wake never goes
		// to the language server; it waits for the next PreInvocation hook,
		// which runs only inside a turn.
		d.mu.Lock()
		d.noWakeHeld[conversationID] = now
		d.mu.Unlock()
		return false, release(errors.New("no_wake waits for a PreInvocation hook"))
	}
	agyToken := d.token()
	if s.LSAddr == "" || agyToken == "" {
		return false, release(errors.New("conversation has no reachable language server"))
	}
	// Read the run state just before the post: a Stop or PreInvocation hook
	// may be on its way, and the language server knows first.
	state, err := d.native.Conversation(ctx, s.LSAddr, agyToken, s.ConversationID)
	if err != nil {
		d.forgetServer(s, err)
		return false, release(fmt.Errorf("read run state: %w", err))
	}
	if stale := postable(action, policy, state.Running()); stale != "" {
		d.log.Info("run state changed before posting; message stays queued", "conversation", s.ConversationID, "delivery", delivery.DeliveryID, "reason", stale)
		return false, release(errors.New(stale))
	}
	ref := delivery.DeliveryID
	if _, err := d.fabric.Begin(ctx, delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref}); err != nil {
		return false, fmt.Errorf("begin dispatch: %w", err)
	}
	pending := &Dispatch{DeliveryID: delivery.DeliveryID, MessageID: claim.Message.MessageID, Ref: ref, ConversationID: s.ConversationID, Proc: s.Proc, Transcript: s.Transcript, Offset: fileSize(s.Transcript), PostedAt: now}
	d.mu.Lock()
	d.dispatches[pending.DeliveryID] = pending
	saveErr := d.saveLocked()
	d.mu.Unlock()
	if saveErr != nil {
		d.log.Warn("save driver state", "error", saveErr)
	}
	err = d.native.Send(ctx, s.LSAddr, agyToken, s.ConversationID, d.render(ctx, *claim.Message))
	var refused *RefusedError
	if errors.Is(err, ErrGone) || errors.As(err, &refused) {
		// Nothing was taken: the process was gone or the server said no.
		d.forgetServer(s, err)
		return false, d.notInserted(ctx, token, claim, ref, "send_refused", err)
	}
	if err != nil {
		// The post may or may not have arrived: the delivery stays dispatching
		// until the conversation's files settle it.
		d.log.Warn("post outcome unclear; settling from the conversation's files", "conversation", s.ConversationID, "delivery", delivery.DeliveryID, "error", err)
		return false, nil
	}
	d.log.Info("posted", "conversation", s.ConversationID, "address", r.Address, "delivery", delivery.DeliveryID, "action", action, "run_state", state.RunState)
	return true, nil
}

// postable says why a claimed delivery must wait instead of being posted now,
// or "" when it can go. A post reaches a running turn at its next step, and
// starts a turn on an idle conversation.
func postable(action, policy string, running bool) string {
	switch action {
	case "deliver_next_step":
		return ""
	case "deliver_at_idle":
		if running && policy != "next_step" {
			return "next_turn target became busy"
		}
		return ""
	}
	return "unsupported dispatch action " + action
}

// forgetServer drops a language server address that stopped answering for
// the driver, so a later hook locates it again.
func (d *Driver) forgetServer(s Session, cause error) {
	var refused *RefusedError
	if !errors.Is(cause, ErrGone) && !(errors.As(cause, &refused) && refused.Code == "unauthenticated") {
		return
	}
	d.mu.Lock()
	if current, ok := d.sessions[s.ConversationID]; ok && current.LSAddr == s.LSAddr {
		current.LSAddr = ""
		_ = d.saveLocked()
	}
	d.mu.Unlock()
}

func (d *Driver) notInserted(ctx context.Context, token string, claim fabricclient.Claim, ref, reason string, cause error) error {
	now := d.now().UTC()
	_, err := d.fabric.NotInserted(ctx, claim.Delivery.DeliveryID, fabricclient.NotInsertedRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("not-inserted", claim.Delivery.DeliveryID, now), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref, Reason: reason})
	d.mu.Lock()
	if err == nil {
		delete(d.dispatches, claim.Delivery.DeliveryID)
	}
	saveErr := d.saveLocked()
	d.mu.Unlock()
	if err != nil {
		return fmt.Errorf("requeue after %v: %w", cause, err)
	}
	d.log.Info("message not taken; left pending as the address head", "delivery", claim.Delivery.DeliveryID, "reason", reason, "cause", cause)
	return saveErr
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

// settleSeatDispatches settles this driver's seat mail still in flight,
// against the conversation each went to.
func (d *Driver) settleSeatDispatches(ctx context.Context) {
	dispatches, err := fabricclient.SeatDispatches(ctx, d.fabric, d.cfg.AdapterID)
	if err != nil {
		d.log.Warn("list seat dispatches", "error", err)
	}
	for _, sd := range dispatches {
		if _, err := d.reconcile(ctx, d.sessionAt(sd.Holder, sd.Delivery.DeliveryID), sd.Delivery); err != nil {
			d.log.Warn("settle seat dispatch", "seat", sd.Delivery.RecipientAddress, "delivery", sd.Delivery.DeliveryID, "error", err)
		}
	}
}

// reconcileHead settles a head this driver left dispatching, against the
// conversation it went to.
func (d *Driver) reconcileHead(ctx context.Context, s Session, head fabricclient.Delivery) (bool, error) {
	if head.ClaimHoldingID == 0 || (head.ClaimOwnerAdapterID != "" && head.ClaimOwnerAdapterID != d.cfg.AdapterID) {
		return d.reconcile(ctx, s, head)
	}
	holder := ""
	d.mu.Lock()
	_, recorded := d.dispatches[head.DeliveryID]
	d.mu.Unlock()
	if !recorded {
		var err error
		if holder, err = fabricclient.HoldingHolder(ctx, d.fabric, head.RecipientAddress, head.ClaimHoldingID); err != nil {
			return false, fmt.Errorf("find the holder of %s: %w", head.DeliveryID, err)
		}
	}
	return d.reconcile(ctx, d.sessionAt(holder, head.DeliveryID), head)
}

// sessionAt is the conversation a seat dispatch went to.
func (d *Driver) sessionAt(holder, deliveryID string) Session {
	d.mu.Lock()
	p, recorded := d.dispatches[deliveryID]
	var kept Session
	if recorded {
		kept = Session{ConversationID: p.ConversationID, Proc: p.Proc, Transcript: p.Transcript}
	}
	d.mu.Unlock()
	if recorded {
		if s, ok := d.session(kept.ConversationID); ok {
			return s
		}
		return kept
	}
	for _, s := range d.snapshot() {
		if s.Address == holder {
			return s
		}
	}
	return Session{}
}

// reconcile settles a dispatching delivery from the conversation's files:
// delivered once the transcript shows it, outcome_unknown when the
// conversation never took it in within AcceptTimeout, took it in and then
// lost its process, or kept it past QueuedTimeout. It never posts again.
func (d *Driver) reconcile(ctx context.Context, s Session, head fabricclient.Delivery) (bool, error) {
	if head.ClaimOwnerAdapterID != "" && head.ClaimOwnerAdapterID != d.cfg.AdapterID {
		return false, nil
	}
	now := d.now().UTC()
	d.mu.Lock()
	pending, ok := d.dispatches[head.DeliveryID]
	if !ok {
		// Posted by an instance that lost its state: look for it from the
		// start of the transcript.
		pending = &Dispatch{DeliveryID: head.DeliveryID, MessageID: head.MessageID, Ref: head.NativeAttemptRef, ConversationID: s.ConversationID, Proc: s.Proc, Transcript: s.Transcript, PostedAt: now}
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
	marker := ""
	if p.MessageID != "" {
		marker = "(crew message " + p.MessageID + ")"
	}
	if !p.Inserted {
		scan, err := scanTranscript(p.Transcript, p.Offset, marker)
		if err != nil {
			return false, fmt.Errorf("read transcript %s: %w", p.Transcript, err)
		}
		p.Offset = scan.Offset
		if scan.Inserted {
			p.Inserted = true
			if err := d.keepDispatch(p); err != nil {
				d.log.Warn("save driver state", "error", err)
			}
		}
	}
	if !p.Inserted && !p.Enqueued && p.Via == "" {
		p.Enqueued = queued(p.Transcript, marker)
	}
	age := now.Sub(p.PostedAt)
	exited := d.procs == nil || !d.procs.Alive(p.Proc)
	request := fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeAttemptRef: p.Ref}
	var outcome string
	var err error
	switch {
	case p.Inserted:
		outcome = "delivered"
		request.OperationID = "crew-agy/ack/" + p.DeliveryID
		_, err = d.fabric.Acknowledge(ctx, p.DeliveryID, request)
	case p.Via == "hook" && age > d.cfg.AcceptTimeout:
		outcome = "outcome_unknown: hook context never reached the transcript"
	case !p.Enqueued && age > d.cfg.AcceptTimeout:
		outcome = "outcome_unknown: never taken in by the conversation"
	case p.Enqueued && exited:
		outcome = "outcome_unknown: taken in, then the agy process exited"
	case p.Enqueued && age > d.cfg.QueuedTimeout:
		outcome = "outcome_unknown: taken in but never entered the conversation"
	}
	if outcome != "" && !p.Inserted {
		request.OperationID = "crew-agy/unknown/" + p.DeliveryID
		_, err = d.fabric.Unknown(ctx, p.DeliveryID, request)
	}
	var gone *fabricclient.HTTPError
	settled := outcome != "" && err == nil
	if outcome != "" && errors.As(err, &gone) && (gone.Status == 404 || gone.Status == 409) {
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
	d.log.Info("settled dispatch", "conversation", p.ConversationID, "delivery", p.DeliveryID, "outcome", outcome, "after", age.Round(time.Millisecond))
	return true, saveErr
}

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

// releaser returns the claim to the queue, even after the caller's context
// ended.
func (d *Driver) releaser(ctx context.Context, token string, claim fabricclient.Claim, now time.Time) func(error) error {
	return func(cause error) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := d.fabric.Release(ctx, claim.Delivery.DeliveryID, fabricclient.ClaimTokenRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("release", claim.Delivery.DeliveryID, now), ClaimToken: claim.ClaimToken}); err != nil {
			return fmt.Errorf("release after %v: %w", cause, err)
		}
		return nil
	}
}

// TurnContext is called by the PreInvocation hook, which runs before each
// model call of a running turn. It marks the conversation busy and returns at
// most one crew message for the hook to inject as a system message, which agy
// adds to that turn and records in the transcript; it can never start a turn.
// This is the only way a no_wake message reaches a conversation, and it also
// carries a next_step message it claims. A message already handed out this
// way is settled from the transcript first, so consecutive model calls drain
// the address in order.
func (d *Driver) TurnContext(ctx context.Context, conversationID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, turnContextBudget)
	defer cancel()
	d.mu.Lock()
	s, ok := d.sessions[conversationID]
	var self Session
	if ok {
		delete(d.noWakeHeld, conversationID)
		s.Busy = true
		s.SeenAt = d.now().UTC()
		_ = d.saveLocked()
		self = *s
	}
	d.mu.Unlock()
	if !ok {
		return "", nil
	}
	for _, r := range d.routes(self) {
		for i := 0; i < 5; i++ {
			text, settled, err := d.turnOne(ctx, conversationID, r)
			if err != nil || text != "" {
				return text, err
			}
			if !settled {
				break
			}
		}
	}
	return "", nil
}

func (d *Driver) turnOne(ctx context.Context, conversationID string, r route) (string, bool, error) {
	s, ok := d.session(conversationID)
	if !ok {
		return "", false, nil
	}
	token := d.leaseToken()
	now := d.now().UTC()
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("claim", r.Address, now), RecipientAddress: r.Address, RecipientGeneration: r.Generation, Availability: "busy", ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return "", false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		if claim.Reason == "head_dispatching" && claim.Head != nil {
			settled, err := d.reconcileHead(ctx, s, *claim.Head)
			return "", settled, err
		}
		return "", false, nil
	}
	delivery := claim.Delivery
	action := claim.DispatchAction
	if action == "" {
		action = delivery.DispatchAction
	}
	release := d.releaser(ctx, token, claim, now)
	if claim.HolderAddress != "" && claim.HolderAddress != s.Address {
		return "", false, release(errors.New("seat " + r.Address + " is now held by " + claim.HolderAddress))
	}
	if action != "deliver_next_step" {
		return "", false, release(errors.New("unexpected dispatch action " + action + " for a running turn"))
	}
	text := d.render(ctx, *claim.Message)
	if ctx.Err() != nil {
		return "", false, release(ctx.Err())
	}
	ref := delivery.DeliveryID
	if _, err := d.fabric.Begin(ctx, delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref}); err != nil {
		return "", false, fmt.Errorf("begin dispatch: %w", err)
	}
	pending := &Dispatch{DeliveryID: delivery.DeliveryID, MessageID: claim.Message.MessageID, Ref: ref, ConversationID: s.ConversationID, Proc: s.Proc, Transcript: s.Transcript, Offset: fileSize(s.Transcript), Via: "hook", PostedAt: now}
	d.mu.Lock()
	d.dispatches[pending.DeliveryID] = pending
	saveErr := d.saveLocked()
	d.mu.Unlock()
	if saveErr != nil {
		d.log.Warn("save driver state", "error", saveErr)
	}
	d.log.Info("handed to the running turn through PreInvocation", "conversation", s.ConversationID, "address", r.Address, "delivery", delivery.DeliveryID, "policy", claim.Message.ActivationPolicy)
	return text, false, nil
}

func (d *Driver) render(ctx context.Context, m fabricclient.Message) string {
	return crewcli.RenderFabricMessage(ctx, m, "crew-agy", d.Lookup)
}

var opSeq atomic.Int64

func opID(kind, subject string, now time.Time) string {
	return fmt.Sprintf("crew-agy/%s/%s/%d-%d", kind, subject, now.UnixNano(), opSeq.Add(1))
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
		d.sessions[s.ConversationID] = s
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
	sort.Slice(list, func(i, j int) bool { return list[i].ConversationID < list[j].ConversationID })
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
