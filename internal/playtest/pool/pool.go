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

type SlotFactory func(index int) (*session.Service, func(), error)

type Pool struct {
	lifecycle context.Context
	cancel    context.CancelFunc
	starts    sync.WaitGroup
	mu        sync.Mutex
	slots     []*session.Service
	capacity  int
	registry  *session.Registry
	factory   SlotFactory
	releases  []func()
	reserved  []bool
	waiters   []uint64
	next      uint64
	changed   chan struct{}
	closed    bool
	wait      time.Duration
	idle      time.Duration
	retention time.Duration
}

// Lifecycle bounds how long a session lives unused and how long an ended
// session's record is kept. Zero disables either.
type Lifecycle struct {
	IdleTimeout time.Duration
	Retention   time.Duration
}

// DefaultLifecycle matches the Engine's idle limit for dev hosts and keeps
// ended session records for two weeks.
var DefaultLifecycle = Lifecycle{IdleTimeout: 30 * time.Minute, Retention: 14 * 24 * time.Hour}

// SetLifecycle publishes the expiry policy the reaper applies from its next
// pass. Running sessions keep their activity clocks.
func (p *Pool) SetLifecycle(l Lifecycle) error {
	if l.IdleTimeout < 0 || l.Retention < 0 {
		return errors.New("idle timeout and retention cannot be negative")
	}
	p.mu.Lock()
	p.idle, p.retention = l.IdleTimeout, l.Retention
	p.mu.Unlock()
	return nil
}

// Reap makes one reaper pass. Each occupied slot is reserved while its
// session is checked, so no recover runs beside it, and ended sessions older
// than the retention are forgotten. It returns one line per session it ended
// or failed to end.
func (p *Pool) Reap(ctx context.Context) []string {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	var picked []int
	for i, s := range p.slots[:p.capacity] {
		if !p.reserved[i] && s.SlotStatus() != nil {
			p.reserved[i] = true
			picked = append(picked, i)
		}
	}
	p.starts.Add(1)
	slots, idle, retention := p.slots, p.idle, p.retention
	p.mu.Unlock()
	defer p.starts.Done()
	reapCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.lifecycle, cancel)
	defer stop()
	var (
		mu    sync.Mutex
		lines []string
		wg    sync.WaitGroup
	)
	for _, i := range picked {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer p.unreserve(i)
			id := ""
			if st := slots[i].SlotStatus(); st != nil {
				id = st.ID
			}
			reason, err := slots[i].Reap(reapCtx, idle)
			if reason == "" && err == nil {
				return
			}
			line := fmt.Sprintf("%s session %s ended: %s", slotID(i), id, reason)
			if err != nil {
				line = fmt.Sprintf("%s session %s could not be ended (%s): %v", slotID(i), id, reason, err)
			}
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if retention > 0 {
		cutoff := time.Now().UTC().Add(-retention)
		for _, s := range slots {
			s.PruneHistory(cutoff)
		}
	}
	return lines
}

// RunReaper reaps at once, which resolves sessions a restart interrupted,
// and then every interval until the pool closes.
func (p *Pool) RunReaper(interval time.Duration, logf func(string, ...any)) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			for _, line := range p.Reap(p.lifecycle) {
				logf("playtest reaper: %s", line)
			}
			select {
			case <-p.lifecycle.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func New(slots []*session.Service, wait time.Duration) (*Pool, error) {
	if len(slots) == 0 {
		return nil, errors.New("pool requires at least one slot")
	}
	if wait < 0 || wait > 20*time.Second {
		return nil, errors.New("queue_wait_ms must be 0..20000")
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	p := &Pool{lifecycle: lifecycle, cancel: cancel, slots: slots, capacity: len(slots), reserved: make([]bool, len(slots)), changed: make(chan struct{}), wait: wait,
		idle: DefaultLifecycle.IdleTimeout, retention: DefaultLifecycle.Retention}
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
	wait := p.wait
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.removeWaiter(ticket); p.mu.Unlock() }()
	deadline := time.NewTimer(wait)
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
			for i, s := range p.slots[:p.capacity] {
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
	slots := make([]map[string]any, 0, p.capacity)
	active := 0
	for i, s := range p.slots[:p.capacity] {
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
	return map[string]any{"observed_at": time.Now().UTC(), "capacity": p.capacity, "active_count": active, "queued_starts": len(p.waiters), "slots": slots}
}
func (p *Pool) Command(ctx context.Context, r session.Request) (any, error) {
	p.mu.Lock()
	closed := p.closed
	slots := p.slots
	p.mu.Unlock()
	if closed && r.Op != "status" && r.Op != "games" && r.Op != "game" {
		return nil, errors.New("pool is shutting down")
	}
	switch r.Op {
	case "games", "game":
		return slots[0].Command(ctx, r)
	case "start":
		if _, err := slots[0].Command(ctx, session.Request{Op: "game", Game: r.Game}); err != nil {
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
		slot := p.slots[index]
		p.mu.Unlock()
		defer p.starts.Done()
		launchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(p.lifecycle, cancel)
		defer stop()
		return slot.Command(launchCtx, r)
	case "status":
		if r.SessionID == "" {
			return p.status(ctx), nil
		}
	}
	index := -1
	for i, s := range slots {
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
		if p.closed || index >= p.capacity || p.reserved[index] {
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
	value, err := slots[index].Command(ctx, r)
	if r.Op == "stop" || r.Op == "cancel" {
		p.mu.Lock()
		p.signalLocked()
		p.mu.Unlock()
	}
	if m, ok := value.(map[string]any); ok && m != nil {
		m["slot_id"] = slotID(index)
	}
	return value, err
}
func (p *Pool) status(ctx context.Context) any {
	p.mu.Lock()
	slots := p.slots[:p.capacity]
	p.mu.Unlock()
	rows := make([]map[string]any, len(slots))
	var wg sync.WaitGroup
	for i, s := range slots {
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
	slots := p.slots
	releases := p.releases
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
	errs := make([]error, len(slots))
	for i, s := range slots {
		wg.Add(1)
		go func(i int, s *session.Service) { defer wg.Done(); errs[i] = s.Close(ctx) }(i, s)
	}
	wg.Wait()
	for _, release := range releases {
		release()
	}
	return errors.Join(errs...)
}

// NewResizable adds live configuration to the same slot allocator. All services
// and the factory must use registry. Construction settings remain in the factory.
func NewResizable(slots []*session.Service, wait time.Duration, registry *session.Registry, factory SlotFactory) (*Pool, error) {
	p, err := New(slots, wait)
	if err != nil {
		return nil, err
	}
	p.registry, p.factory = registry, factory
	return p, nil
}

// Reload publishes the registry, capacity and queue policy together. Inactive
// tail slots retain their history and can be reused on growth; they cannot start
// or recover a session while outside capacity. No live slot is renumbered.
func (p *Pool) Reload(profiles []session.Profile, size int, wait time.Duration) error {
	if err := session.ValidateProfiles(profiles); err != nil {
		return err
	}
	if size < 1 {
		return errors.New("pool size must be positive")
	}
	if wait < 0 || wait > 20*time.Second {
		return errors.New("queue_wait_ms must be 0..20000")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("pool is shutting down")
	}
	if p.registry == nil {
		return errors.New("pool reload is not configured")
	}
	for i := size; i < p.capacity; i++ {
		if p.reserved[i] || p.slots[i].SlotStatus() != nil {
			return fmt.Errorf("pool_shrink_busy: %s is occupied; stop it before shrinking", slotID(i))
		}
	}
	var added []*session.Service
	var releases []func()
	for i := len(p.slots); i < size; i++ {
		if p.factory == nil {
			return errors.New("pool growth is not configured")
		}
		slot, release, err := p.factory(i)
		if err != nil {
			for _, cleanup := range releases {
				cleanup()
			}
			return fmt.Errorf("create %s: %w", slotID(i), err)
		}
		slot.ConfigureSlot(slotID(i), p.Activity)
		added = append(added, slot)
		releases = append(releases, release)
	}
	// Validation above means Replace cannot fail. Publish only after every new
	// slot is ready; no backend selector or running session is touched.
	if err := p.registry.Replace(profiles); err != nil {
		for _, cleanup := range releases {
			cleanup()
		}
		return err
	}
	p.slots = append(p.slots, added...)
	p.reserved = append(p.reserved, make([]bool, len(added))...)
	p.releases = append(p.releases, releases...)
	p.capacity, p.wait = size, wait
	p.signalLocked()
	return nil
}
