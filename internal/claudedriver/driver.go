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
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

// Capabilities this driver advertises on every session binding. It cannot
// start a Claude Code process, so it never claims wake_inactive.
var Capabilities = []string{"deliver_next_step", "deliver_when_idle", "durable_next_turn"}

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
}

// Session is the driver-private record for one Claude Code session. The
// socket path and token change whenever the process restarts, so they live
// here rather than in the fabric binding.
type Session struct {
	SessionID  string    `json:"session_id"`
	Socket     string    `json:"socket"`
	Token      string    `json:"token,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Address    string    `json:"address"`
	Generation int64     `json:"generation"`
	Busy       bool      `json:"busy"`
	SeenAt     time.Time `json:"seen_at"`
}

type Driver struct {
	fabric Fabric
	inbox  Inbox
	now    func() time.Time
	cfg    Config
	log    *slog.Logger

	mu       sync.Mutex
	lease    fabricclient.Lease
	sessions map[string]*Session
	pumpMu   sync.Mutex
	wake     chan struct{}
}

func New(fabric Fabric, inbox Inbox, now func() time.Time, cfg Config, log *slog.Logger) (*Driver, error) {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.ClaimDuration == 0 {
		cfg.ClaimDuration = time.Minute
	}
	d := &Driver{fabric: fabric, inbox: inbox, now: now, cfg: cfg, log: log, sessions: map[string]*Session{}, wake: make(chan struct{}, 1)}
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
	return nil
}

func (d *Driver) leaseToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease.LeaseToken
}

// RegisterSession records a session's inbox and returns its crew address.
func (d *Driver) RegisterSession(ctx context.Context, sessionID, socket, token, cwd string) (string, error) {
	if sessionID == "" || socket == "" {
		return "", errors.New("session_id and socket are required")
	}
	binding, err := d.fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: d.cfg.AdapterID, LeaseToken: d.leaseToken(), NativeKey: sessionID, Capabilities: Capabilities})
	if err != nil {
		return "", fmt.Errorf("register session address: %w", err)
	}
	d.mu.Lock()
	d.sessions[sessionID] = &Session{SessionID: sessionID, Socket: socket, Token: token, Cwd: cwd, Address: binding.Address, Generation: binding.Generation, SeenAt: d.now().UTC()}
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
// session's eligible work in order.
func (d *Driver) Pump(ctx context.Context) {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	for _, s := range d.snapshot() {
		for i := 0; i < 20; i++ {
			delivered, err := d.deliverOne(ctx, s)
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

func (d *Driver) availability(s Session) string {
	if _, err := os.Stat(s.Socket); err != nil {
		return "inactive"
	}
	if s.Busy {
		return "busy"
	}
	return "idle"
}

// deliverOne claims and delivers at most one message for s. It reports whether
// a message went out, so the caller keeps draining.
func (d *Driver) deliverOne(ctx context.Context, s Session) (bool, error) {
	token := d.leaseToken()
	now := d.now().UTC()
	claim, err := d.fabric.Claim(ctx, fabricclient.ClaimRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("claim", s.Address, now), RecipientAddress: s.Address, RecipientGeneration: s.Generation, Availability: d.availability(s), ClaimDuration: d.cfg.ClaimDuration.String()})
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil {
		return false, nil
	}
	delivery := claim.Delivery
	priority := PriorityLater
	if claim.DispatchAction == "deliver_next_step" {
		priority = PriorityNext
	}
	// Open the socket before recording the dispatch: a session that is gone
	// sends the claim back to the queue instead of leaving it unknown.
	conn, err := d.inbox.Dial(ctx, s.Socket, s.Token)
	if err != nil {
		if _, releaseErr := d.fabric.Release(ctx, delivery.DeliveryID, fabricclient.ClaimTokenRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("release", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken}); releaseErr != nil {
			return false, fmt.Errorf("release after %v: %w", err, releaseErr)
		}
		return false, nil
	}
	defer conn.Close()
	ref := delivery.DeliveryID
	if _, err := d.fabric.Begin(ctx, delivery.DeliveryID, fabricclient.DispatchRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("begin", delivery.DeliveryID, now), ClaimToken: claim.ClaimToken, NativeAttemptRef: ref}); err != nil {
		return false, fmt.Errorf("begin dispatch: %w", err)
	}
	if err := conn.Post(nativeMessageID(delivery.DeliveryID), Render(*claim.Message), priority); err != nil {
		if _, unknownErr := d.fabric.Unknown(ctx, delivery.DeliveryID, fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("unknown", delivery.DeliveryID, now), NativeAttemptRef: ref}); unknownErr != nil {
			return false, fmt.Errorf("outcome unknown after %v: %w", err, unknownErr)
		}
		return false, fmt.Errorf("post: %w", err)
	}
	if _, err := d.fabric.Acknowledge(ctx, delivery.DeliveryID, fabricclient.ReconcileRequest{AdapterID: d.cfg.AdapterID, LeaseToken: token, OperationID: opID("ack", delivery.DeliveryID, now), NativeAttemptRef: ref}); err != nil {
		return true, fmt.Errorf("acknowledge: %w", err)
	}
	d.log.Info("delivered", "session", s.SessionID, "address", s.Address, "delivery", delivery.DeliveryID, "priority", priority)
	return true, nil
}

// Render is the text the Claude session reads. It says where the message came
// from so the receiving agent can treat it as crew traffic, not its user.
func Render(m fabricclient.Message) string {
	return fmt.Sprintf("Crew message from %s (crew message %s):\n\n%s", m.SenderAddress, m.MessageID, strings.TrimSpace(m.Body))
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

func opID(kind, subject string, now time.Time) string {
	return fmt.Sprintf("crew-claude/%s/%s/%d", kind, subject, now.UnixNano())
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
		Sessions []*Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse driver state: %w", err)
	}
	for _, s := range stored.Sessions {
		d.sessions[s.SessionID] = s
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
	data, err := json.MarshalIndent(struct {
		Sessions []*Session `json:"sessions"`
	}{list}, "", "  ")
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
