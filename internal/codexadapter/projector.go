package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const projectionEventType = "runtime.entry.completed"

const promptDeliveryCapability = "queued-prompt-delivery"

var codexCapabilities = []string{
	"session-events", "read-only-native-history",
	"deliver_when_idle", "durable_next_turn", promptDeliveryCapability,
}

// Projector connects explicit native threads to their fabric-owned public
// sessions. It holds no native state: an adapter restart can reconstruct every
// projection from canonical thread/read responses and fabric idempotency.
type Projector struct {
	Fabric        Fabric
	Native        AppServer
	AdapterID     string
	Lease         Lease
	Mappings      []Mapping
	Capabilities  []string
	ClaimDuration time.Duration
}

func (p *Projector) Reconcile(ctx context.Context) error {
	if p.Fabric == nil || p.Native == nil {
		return errors.New("fabric and native App Server are required")
	}
	if strings.TrimSpace(p.Lease.LeaseToken) == "" {
		return errors.New("adapter lease is required")
	}
	for _, mapping := range p.Mappings {
		// thread/list is paginated and intentionally not an adoption authority.
		// The explicit mapping gives us an exact identity; thread/read is the
		// canonical metadata/history source for that identity.
		thread, err := p.Native.ReadThread(ctx, mapping.ThreadID)
		if err != nil {
			return fmt.Errorf("read Codex thread %q: %w", mapping.ThreadID, err)
		}
		if thread.ID != mapping.ThreadID {
			return fmt.Errorf("thread/read identity mismatch: configured %q, returned %q", mapping.ThreadID, thread.ID)
		}
		if err := p.projectThread(ctx, mapping, thread); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) projectThread(ctx context.Context, mapping Mapping, thread NativeThread) error {
	session, err := p.Fabric.Adopt(ctx, AdoptRequest{
		AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, AdapterKey: thread.ID,
		Label: threadLabel(thread), Location: thread.CWD, Status: threadStatus(thread.Status), Capabilities: p.Capabilities,
	})
	if err != nil {
		return fmt.Errorf("adopt %q: %w", mapping.Address, err)
	}
	binding, err := p.bind(ctx, mapping.Address, session)
	if err != nil {
		return err
	}
	if session, err = p.updateIfChanged(ctx, session, thread); err != nil {
		return err
	}
	for _, event := range normalizedEvents(thread) {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode native entry %q: %w", event.EntryID, err)
		}
		if err := p.Fabric.Append(ctx, session.SessionID, AppendRequest{
			AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, ExpectedRevision: session.Revision,
			OperationID: event.operationID(thread.ID), EventType: projectionEventType, Payload: payload,
		}); err != nil {
			return fmt.Errorf("append native entry %q: %w", event.EntryID, err)
		}
	}
	if err := p.reconcileDispatching(ctx, mapping, thread, binding); err != nil {
		return err
	}
	return p.deliverQueued(ctx, mapping, thread, binding)
}

func (p *Projector) bind(ctx context.Context, address string, session Session) (Binding, error) {
	binding, err := p.Fabric.Resolve(ctx, address)
	if err != nil {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != 404 {
			return Binding{}, fmt.Errorf("resolve %q: %w", address, err)
		}
		binding = Binding{Address: address}
	}
	if binding.Bound {
		if binding.AdapterID != p.AdapterID || binding.TargetRef != session.SessionID {
			return Binding{}, fmt.Errorf("address %q is owned by %q and cannot be rebound", address, binding.AdapterID)
		}
		if equalStrings(binding.Capabilities, p.Capabilities) {
			return binding, nil
		}
	}
	var expected *int64
	if binding.Revision > 0 {
		expected = &binding.Revision
	}
	written, err := p.Fabric.Bind(ctx, address, BindRequest{
		ActorAdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, AdapterID: p.AdapterID, TargetRef: session.SessionID,
		Capabilities: p.Capabilities, ExpectedRevision: expected,
	})
	if err != nil {
		return Binding{}, fmt.Errorf("bind %q: %w", address, err)
	}
	return written, nil
}

// deliverQueued makes only the native queue insertion. Queue admission is
// durable and FIFO; Codex starts the queued work itself when the thread is
// idle, so this adapter never needs to inspect-idle then call turn/start.
func (p *Projector) deliverQueued(ctx context.Context, mapping Mapping, thread NativeThread, binding Binding) error {
	// Codex has no inactive-thread wake primitive in this adapter. Do not even
	// record a no-work claim: its availability is part of the durable receipt
	// fingerprint and would conflict with the later idle claim for this head.
	if threadAvailability(thread.Status) == "inactive" {
		return nil
	}
	head, err := p.deliveryHead(ctx, mapping.Address, binding.Generation)
	if err != nil {
		return err
	}
	if head == nil {
		return nil
	}
	availability, ok := claimAvailability(*head, thread.Status, p.AdapterID, p.Lease.InstanceID)
	if !ok {
		return nil
	}
	claimAttempt := head.AttemptCount + 1
	if head.State == "claimed" {
		claimAttempt = head.AttemptCount
	}
	request := ClaimRequest{
		AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken,
		OperationID:      claimOperation(head.DeliveryID, claimAttempt),
		RecipientAddress: mapping.Address, RecipientGeneration: binding.Generation,
		Availability: availability, ClaimDuration: p.ClaimDuration.String(),
	}
	claim, err := p.Fabric.Claim(ctx, request)
	if err != nil {
		// This is intentionally a same-operation replay: it either receives the
		// committed claim receipt (including its private token) or executes the
		// one durable claim that the lost response left ambiguous.
		claim, err = p.Fabric.Claim(ctx, request)
		if err != nil {
			return fmt.Errorf("claim %q: %w", mapping.Address, err)
		}
	}
	if !claim.Claimed || claim.Delivery == nil || claim.Message == nil || claim.ClaimToken == "" {
		return nil
	}
	delivery, envelope := *claim.Delivery, *claim.Message
	attempt, clientID := nativeAttempt(delivery.DeliveryID), clientMessageID(delivery.DeliveryID)
	if _, err := p.Fabric.Begin(ctx, delivery.DeliveryID, DispatchRequest{
		AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "begin"),
		ClaimToken: claim.ClaimToken, NativeAttemptRef: attempt,
	}); err != nil {
		return fmt.Errorf("begin dispatch %q: %w", delivery.DeliveryID, err)
	}
	queued, queueErr := p.Native.QueueAdd(ctx, mapping.ThreadID, envelope.Body, clientID)
	if queueErr == nil && queued.ID != "" && queued.ClientUserMessageID == clientID {
		if _, err := p.Fabric.Acknowledge(ctx, delivery.DeliveryID, ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "ack"), NativeAttemptRef: attempt}); err != nil {
			return fmt.Errorf("acknowledge %q: %w", delivery.DeliveryID, err)
		}
		return nil
	}
	// A lost native response is ambiguous. Read durable queue/history before
	// settling rather than replaying the prompt into a possibly active thread.
	accepted, readErr := p.nativeAccepted(ctx, mapping.ThreadID, thread, clientID)
	if readErr != nil {
		if _, unknownErr := p.Fabric.Unknown(ctx, delivery.DeliveryID, ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "unknown"), NativeAttemptRef: attempt}); unknownErr != nil {
			return fmt.Errorf("settle unknown after native observation %q: %w", delivery.DeliveryID, unknownErr)
		}
		return fmt.Errorf("reconcile native queue %q: %w", delivery.DeliveryID, readErr)
	}
	if accepted {
		if _, err := p.Fabric.Acknowledge(ctx, delivery.DeliveryID, ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "ack"), NativeAttemptRef: attempt}); err != nil {
			return fmt.Errorf("acknowledge reconciled %q: %w", delivery.DeliveryID, err)
		}
		return nil
	}
	if _, err := p.Fabric.Unknown(ctx, delivery.DeliveryID, ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "unknown"), NativeAttemptRef: attempt}); err != nil {
		return fmt.Errorf("settle unknown %q: %w", delivery.DeliveryID, err)
	}
	if queueErr != nil {
		return fmt.Errorf("queue native prompt %q: %w", delivery.DeliveryID, queueErr)
	}
	return fmt.Errorf("queue native prompt %q returned an invalid receipt", delivery.DeliveryID)
}

// reconcileDispatching settles only the adapter's own exact native attempt.
// It is deliberately an observation pass: missing proof becomes
// outcome_unknown, never a second queue/add request.
func (p *Projector) reconcileDispatching(ctx context.Context, mapping Mapping, thread NativeThread, binding Binding) error {
	deliveries, err := p.Fabric.Deliveries(ctx)
	if err != nil {
		return fmt.Errorf("list dispatching deliveries: %w", err)
	}
	for _, delivery := range deliveries {
		if delivery.State != "dispatching" || delivery.ClaimOwnerAdapterID != p.AdapterID || delivery.RecipientAddress != mapping.Address || delivery.RecipientGeneration != binding.Generation || delivery.NativeAttemptRef != nativeAttempt(delivery.DeliveryID) {
			continue
		}
		accepted, err := p.nativeAccepted(ctx, mapping.ThreadID, thread, clientMessageID(delivery.DeliveryID))
		if err != nil {
			request := ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, OperationID: deliveryOperation(delivery.DeliveryID, "unknown"), NativeAttemptRef: delivery.NativeAttemptRef}
			if _, unknownErr := p.Fabric.Unknown(ctx, delivery.DeliveryID, request); unknownErr != nil {
				return fmt.Errorf("settle unavailable native dispatch %q: %w", delivery.DeliveryID, unknownErr)
			}
			continue
		}
		request := ReconcileRequest{AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, NativeAttemptRef: delivery.NativeAttemptRef}
		if accepted {
			request.OperationID = deliveryOperation(delivery.DeliveryID, "ack")
			if _, err := p.Fabric.Acknowledge(ctx, delivery.DeliveryID, request); err != nil {
				return fmt.Errorf("acknowledge recovered %q: %w", delivery.DeliveryID, err)
			}
			continue
		}
		request.OperationID = deliveryOperation(delivery.DeliveryID, "unknown")
		if _, err := p.Fabric.Unknown(ctx, delivery.DeliveryID, request); err != nil {
			return fmt.Errorf("settle recovered %q: %w", delivery.DeliveryID, err)
		}
	}
	return nil
}

// deliveryHead observes the one exact binding-generation FIFO head before
// forming its durable claim operation identity. A claimed head may be replayed
// only by the same adapter instance, which recovers its stored claim token.
func (p *Projector) deliveryHead(ctx context.Context, address string, generation int64) (*Delivery, error) {
	deliveries, err := p.Fabric.Deliveries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list delivery head %q: %w", address, err)
	}
	var head *Delivery
	for _, delivery := range deliveries {
		if delivery.RecipientAddress != address || delivery.RecipientGeneration != generation || (delivery.State != "queued" && delivery.State != "claimed") {
			continue
		}
		// The service will terminalize an expired queued head before a later
		// eligible delivery. Do not attach the later claim receipt to that
		// already-expired delivery's operation identity.
		if delivery.State == "queued" && !delivery.ExpiresAt.IsZero() && !delivery.ExpiresAt.After(time.Now()) {
			continue
		}
		if head == nil || delivery.AcceptedSequence < head.AcceptedSequence {
			candidate := delivery
			head = &candidate
		}
	}
	return head, nil
}

func claimAvailability(head Delivery, threadStatus, adapterID, instanceID string) (string, bool) {
	switch head.State {
	case "queued":
		return threadAvailability(threadStatus), true
	case "claimed":
		if head.ClaimOwnerAdapterID != adapterID || head.ClaimOwnerInstanceID != instanceID {
			return "", false
		}
		switch head.DispatchAction {
		case "register_next_turn":
			return "busy", true
		case "deliver_at_idle":
			return "idle", true
		default:
			return "", false
		}
	default:
		return "", false
	}
}

func (p *Projector) nativeAccepted(ctx context.Context, threadID string, thread NativeThread, clientID string) (bool, error) {
	if threadHasClientID(thread, clientID) {
		return true, nil
	}
	queued, err := p.Native.QueueList(ctx, threadID)
	if err != nil {
		return false, err
	}
	if containsQueuedClientID(queued, clientID) {
		return true, nil
	}
	// queue/add may hand an idle thread straight to its next turn. Re-read the
	// canonical thread after observing an empty queue so that this handoff is
	// not mistaken for an unproven native dispatch.
	refreshed, err := p.Native.ReadThread(ctx, threadID)
	if err != nil {
		// The queue read already established that this attempt is not pending.
		// A failed best-effort race read leaves it unproven, which the caller
		// settles as outcome_unknown rather than retrying it.
		return false, nil
	}
	return threadHasClientID(refreshed, clientID), nil
}

func threadAvailability(status string) string {
	if status == "active" {
		return "busy"
	}
	if status == "idle" {
		return "idle"
	}
	return "inactive"
}
func nativeAttempt(deliveryID string) string   { return "codex-queue:" + deliveryID }
func clientMessageID(deliveryID string) string { return "crew-delivery:" + deliveryID }
func claimOperation(deliveryID string, attempt int) string {
	return fmt.Sprintf("crew-codex:%s:claim:%d", deliveryID, attempt)
}
func deliveryOperation(deliveryID, action string) string {
	return "crew-codex:" + deliveryID + ":" + action
}
func containsQueuedClientID(values []QueuedSubmission, clientID string) bool {
	for _, value := range values {
		if value.ClientUserMessageID == clientID {
			return true
		}
	}
	return false
}
func threadHasClientID(thread NativeThread, clientID string) bool {
	for _, turn := range thread.Turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID == clientID {
				return true
			}
		}
	}
	return false
}

func (p *Projector) updateIfChanged(ctx context.Context, session Session, thread NativeThread) (Session, error) {
	label, location, status := threadLabel(thread), thread.CWD, threadStatus(thread.Status)
	if session.Label == label && session.Location == location && session.Status == status && equalStrings(session.Capabilities, p.Capabilities) {
		return session, nil
	}
	updated, err := p.Fabric.Update(ctx, UpdateRequest{
		SessionID: session.SessionID, AdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, ExpectedRevision: session.Revision,
		Label: label, Location: location, Status: status, Capabilities: p.Capabilities,
	})
	if err != nil {
		return Session{}, fmt.Errorf("update session %q: %w", session.SessionID, err)
	}
	return updated, nil
}

func threadLabel(thread NativeThread) string {
	if strings.TrimSpace(thread.Name) != "" {
		return strings.TrimSpace(thread.Name)
	}
	if strings.TrimSpace(thread.ID) != "" {
		return "Codex session"
	}
	return "Codex session"
}
func threadStatus(status string) string {
	switch status {
	case "active":
		return "active"
	case "idle":
		return "idle"
	case "notLoaded":
		return "inactive"
	case "systemError":
		return "error"
	default:
		return "unknown"
	}
}
func equalStrings(a, b []string) bool {
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

// Entry is the durable, runtime-independent projection subset. It is not a
// native transcript format: unsupported items (tools, shell output, reasoning,
// and partial activity) are deliberately omitted from this first slice.
type Entry struct {
	EntryID   string `json:"entry_id"`
	TurnID    string `json:"turn_id"`
	Author    string `json:"author"`
	Category  string `json:"category"`
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
	Final     bool   `json:"final"`
}

func (e Entry) operationID(threadID string) string {
	return "codex-entry:" + threadID + ":" + e.TurnID + ":" + e.EntryID
}

func normalizedEvents(thread NativeThread) []Entry {
	entries := make([]Entry, 0)
	for _, turn := range thread.Turns {
		completed := turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted"
		for _, item := range turn.Items {
			if item.ID == "" {
				continue
			}
			switch item.Type {
			case "userMessage":
				entries = append(entries, Entry{EntryID: item.ID, TurnID: turn.ID, Author: "user", Category: "message", Text: userText(item), Completed: true, Final: true})
			case "agentMessage":
				if !completed {
					continue
				}
				entries = append(entries, Entry{EntryID: item.ID, TurnID: turn.ID, Author: "agent", Category: "message", Text: item.Text, Completed: true, Final: turn.Status == "completed"})
			}
		}
	}
	return entries
}
func userText(item NativeItem) string {
	parts := make([]string, 0, len(item.Content))
	for _, part := range item.Content {
		if part.Type == "text" && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Run maintains a lease and restarts the native child after a failed canonical
// read. Backoff is bounded by the polling cadence, so no tight failure spin is
// possible and shutdown remains prompt.
func Run(ctx context.Context, cfg Config, fabric Fabric, open func() (AppServer, error), logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	lease, err := fabric.Register(ctx, cfg.AdapterID, cfg.InstanceID, cfg.LeaseDuration)
	if err != nil {
		return fmt.Errorf("register fabric adapter: %w", err)
	}
	var native AppServer
	defer func() {
		if native != nil {
			_ = native.Close()
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		lease, err = maintainLease(ctx, fabric, cfg, lease, time.Now())
		if err != nil {
			logf("crew-codex: lease maintenance failed: %v", err)
			if !wait(ctx, cfg.PollInterval) {
				return nil
			}
			continue
		}
		if native == nil {
			native, err = open()
			if err == nil {
				err = native.Initialize(ctx)
			}
			if err != nil {
				logf("crew-codex: native App Server unavailable: %v", err)
				if native != nil {
					_ = native.Close()
					native = nil
				}
				if !wait(ctx, cfg.PollInterval) {
					return nil
				}
				continue
			}
		}
		projector := Projector{Fabric: fabric, Native: native, AdapterID: cfg.AdapterID, Lease: lease, Mappings: cfg.Mappings, Capabilities: codexCapabilities, ClaimDuration: cfg.ClaimDuration}
		if err := projector.Reconcile(ctx); err != nil {
			logf("crew-codex: reconciliation failed: %v", err)
			_ = native.Close()
			native = nil
		}
		if !wait(ctx, cfg.PollInterval) {
			return nil
		}
	}
}

// maintainLease is intentionally independent of native reconciliation. A
// long-lived App Server outage must not let a stable adapter identity become
// permanently fenced before that native runtime can come back.
func maintainLease(ctx context.Context, fabric Fabric, cfg Config, lease Lease, now time.Time) (Lease, error) {
	if !lease.ExpiresAt.IsZero() && lease.ExpiresAt.After(now.Add(cfg.LeaseDuration/2)) {
		return lease, nil
	}
	renewed, renewErr := fabric.Renew(ctx, cfg.AdapterID, lease.LeaseToken, cfg.LeaseDuration)
	if renewErr == nil {
		return renewed, nil
	}
	// Same stable instance ID lets the fabric recover a lease that expired while
	// this process was waiting for a native runtime. It is not a takeover path:
	// another live instance remains fenced by the fabric's registration rules.
	recovered, registerErr := fabric.Register(ctx, cfg.AdapterID, cfg.InstanceID, cfg.LeaseDuration)
	if registerErr == nil {
		return recovered, nil
	}
	return Lease{}, fmt.Errorf("renew lease: %w; recover registration: %v", renewErr, registerErr)
}
func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
