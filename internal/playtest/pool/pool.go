// Package pool allocates independent playtest session services. Each slot keeps
// its existing backend, script, cancellation and durable recovery boundaries.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"crew-services/internal/playtest/session"
)

type Pool struct {
	lifecycle context.Context
	cancel    context.CancelFunc
	starts    sync.WaitGroup
	mu        sync.Mutex
	slots     []*session.Service
	reserved  []bool
	waiters   []uint64
	next      uint64
	changed   chan struct{}
	closed    bool
	wait      time.Duration
}

func New(slots []*session.Service, wait time.Duration) (*Pool, error) {
	if len(slots) == 0 {
		return nil, errors.New("pool requires at least one slot")
	}
	if wait < 0 || wait > 20*time.Second {
		return nil, errors.New("queue_wait_ms must be 0..20000")
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	p := &Pool{lifecycle: lifecycle, cancel: cancel, slots: slots, reserved: make([]bool, len(slots)), changed: make(chan struct{}), wait: wait}
	for i, s := range slots {
		if s == nil {
			return nil, errors.New("pool slot is nil")
		}
		s.ConfigureSlot(slotID(i), p.Activity)
	}
	return p, nil
}
func slotID(i int) string       { return fmt.Sprintf("slot-%d", i+1) }
func (p *Pool) signalLocked()   { close(p.changed); p.changed = make(chan struct{}) }
func (p *Pool) unreserve(i int) { p.mu.Lock(); p.reserved[i] = false; p.signalLocked(); p.mu.Unlock() }
func (p *Pool) removeWaiter(id uint64) {
	for n, v := range p.waiters {
		if v == id {
			p.waiters = append(p.waiters[:n], p.waiters[n+1:]...)
			p.signalLocked()
			return
		}
	}
}
func (p *Pool) reserve(ctx context.Context) (int, error) {
	p.mu.Lock()
	p.next++
	ticket := p.next
	p.waiters = append(p.waiters, ticket)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.removeWaiter(ticket); p.mu.Unlock() }()
	deadline := time.NewTimer(p.wait)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return -1, errors.New("pool is shutting down")
		}
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return -1, err
		}
		if len(p.waiters) > 0 && p.waiters[0] == ticket {
			for i, s := range p.slots {
				if !p.reserved[i] && s.SlotStatus() == nil {
					p.reserved[i] = true
					p.removeWaiter(ticket)
					p.mu.Unlock()
					return i, nil
				}
			}
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-deadline.C:
			return -1, errors.New("pool_busy: all playtest slots are occupied; inspect status and retry later")
		case <-changed:
		}
	}
}

// Activity records occupancy only. An idle script's game can still render, so
// this metadata is not a promise of exclusive GPU time.
func (p *Pool) Activity() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := make([]map[string]any, 0, len(p.slots))
	active := 0
	for i, s := range p.slots {
		st := s.SlotStatus()
		busy := p.reserved[i] || st != nil
		if busy {
			active++
		}
		row := map[string]any{"slot_id": slotID(i), "occupied": busy}
		if st != nil {
			row["session_id"] = st.ID
			row["game"] = st.Game
			row["phase"] = st.Phase
		}
		slots = append(slots, row)
	}
	return map[string]any{"observed_at": time.Now().UTC(), "capacity": len(p.slots), "active_count": active, "queued_starts": len(p.waiters), "slots": slots}
}
func (p *Pool) Command(ctx context.Context, r session.Request) (any, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed && r.Op != "status" && r.Op != "games" && r.Op != "game" {
		return nil, errors.New("pool is shutting down")
	}
	switch r.Op {
	case "games", "game":
		return p.slots[0].Command(ctx, r)
	case "start":
		if _, err := p.slots[0].Command(ctx, session.Request{Op: "game", Game: r.Game}); err != nil {
			return nil, err
		}
		index, err := p.reserve(ctx)
		if err != nil {
			return p.Activity(), err
		}
		defer p.unreserve(index)
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool is shutting down")
		}
		p.starts.Add(1)
		p.mu.Unlock()
		defer p.starts.Done()
		launchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(p.lifecycle, cancel)
		defer stop()
		return p.slots[index].Command(launchCtx, r)
	case "status":
		if r.SessionID == "" {
			return p.status(ctx), nil
		}
	}
	index := -1
	for i, s := range p.slots {
		if (r.Op == "script" && s.OwnsScript(r.ScriptID)) || (r.Op != "script" && r.SessionID != "" && s.OwnsSession(r.SessionID)) {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, errors.New("unknown session or script; use status to discover pool sessions")
	}
	if r.Op == "recover" {
		p.mu.Lock()
		if p.closed || p.reserved[index] {
			p.mu.Unlock()
			return nil, errors.New("slot transition in progress")
		}
		p.reserved[index] = true
		p.starts.Add(1)
		p.mu.Unlock()
		defer p.starts.Done()
		defer p.unreserve(index)
		transitionCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(p.lifecycle, cancel)
		defer stop()
		ctx = transitionCtx
	}
	value, err := p.slots[index].Command(ctx, r)
	if r.Op == "stop" || r.Op == "cancel" {
		p.mu.Lock()
		p.signalLocked()
		p.mu.Unlock()
	}
	if m, ok := value.(map[string]any); ok {
		m["slot_id"] = slotID(index)
	}
	return value, err
}
func (p *Pool) status(ctx context.Context) any {
	rows := make([]map[string]any, len(p.slots))
	var wg sync.WaitGroup
	for i, s := range p.slots {
		wg.Add(1)
		go func(i int, s *session.Service) {
			defer wg.Done()
			v, err := s.Status(ctx, "")
			m, _ := v.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			m["slot_id"] = slotID(i)
			if err != nil {
				m["error"] = err.Error()
			}
			rows[i] = m
		}(i, s)
	}
	wg.Wait()
	return map[string]any{"pool": p.Activity(), "slots": rows}
}
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.signalLocked()
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.starts.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	var wg sync.WaitGroup
	errs := make([]error, len(p.slots))
	for i, s := range p.slots {
		wg.Add(1)
		go func(i int, s *session.Service) { defer wg.Done(); errs[i] = s.Close(ctx) }(i, s)
	}
	wg.Wait()
	return errors.Join(errs...)
}
