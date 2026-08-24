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

// Projector connects explicit native threads to their fabric-owned public
// sessions. It holds no native state: an adapter restart can reconstruct every
// projection from canonical thread/read responses and fabric idempotency.
type Projector struct {
	Fabric       Fabric
	Native       AppServer
	AdapterID    string
	Lease        Lease
	Mappings     []Mapping
	Capabilities []string
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
	if err := p.bind(ctx, mapping.Address, session); err != nil {
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
	return nil
}

func (p *Projector) bind(ctx context.Context, address string, session Session) error {
	binding, err := p.Fabric.Resolve(ctx, address)
	if err != nil {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != 404 {
			return fmt.Errorf("resolve %q: %w", address, err)
		}
		binding = Binding{Address: address}
	}
	if binding.Bound {
		if binding.AdapterID != p.AdapterID || binding.TargetRef != session.SessionID {
			return fmt.Errorf("address %q is owned by %q and cannot be rebound", address, binding.AdapterID)
		}
		return nil
	}
	var expected *int64
	if binding.Revision > 0 {
		expected = &binding.Revision
	}
	_, err = p.Fabric.Bind(ctx, address, BindRequest{
		ActorAdapterID: p.AdapterID, LeaseToken: p.Lease.LeaseToken, AdapterID: p.AdapterID, TargetRef: session.SessionID,
		Capabilities: p.Capabilities, ExpectedRevision: expected,
	})
	if err != nil {
		return fmt.Errorf("bind %q: %w", address, err)
	}
	return nil
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
		projector := Projector{Fabric: fabric, Native: native, AdapterID: cfg.AdapterID, Lease: lease, Mappings: cfg.Mappings, Capabilities: []string{"session-events", "read-only-native-history"}}
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
