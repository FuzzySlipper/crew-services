package humans

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Pusher sends a notice over one kind of route.
type Pusher interface {
	Push(ctx context.Context, person Person, route RouteConfig, n Notice) error
}

// Refused is a definite answer from a route (a bad token, an unknown notify
// service): retrying cannot help, so the delivery fails at once.
type Refused struct{ Err error }

func (r Refused) Error() string { return "refused: " + r.Err.Error() }
func (r Refused) Unwrap() error { return r.Err }

// Deliverer pushes due deliveries. Whatever happens is recorded on the
// delivery; the notice stays in the inbox either way.
type Deliverer struct {
	svc     *Service
	store   Store
	pushers map[string]Pusher
	now     func() time.Time
	wake    chan struct{}
	mu      sync.Mutex
}

func NewDeliverer(svc *Service, store Store, pushers map[string]Pusher, now func() time.Time) *Deliverer {
	return &Deliverer{svc: svc, store: store, pushers: pushers, now: now, wake: make(chan struct{}, 1)}
}

// Poke asks for a pass now.
func (d *Deliverer) Poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run makes a pass on every tick and whenever poked, until ctx ends.
func (d *Deliverer) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

// Pass attempts every due delivery once, and reports how many settled.
func (d *Deliverer) Pass(ctx context.Context) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	due, err := d.store.DueDeliveries(ctx, d.now().UTC(), 50)
	if err != nil {
		return 0
	}
	settled := 0
	for _, delivery := range due {
		if d.attempt(ctx, delivery) {
			settled++
		}
	}
	return settled
}

func (d *Deliverer) attempt(ctx context.Context, delivery Delivery) bool {
	now := d.now().UTC()
	delivery.Attempts++
	notice, err := d.store.GetNotice(ctx, delivery.NoticeID)
	if err != nil {
		return false
	}
	person, route, ok := d.svc.route(notice.Person, delivery)
	pusher := d.pushers[delivery.Kind]
	switch {
	case !ok:
		delivery.State, delivery.LastError = DeliveryFailed, "the route is no longer in the people file"
	case pusher == nil:
		delivery.State, delivery.LastError = DeliveryFailed, "no pusher for "+delivery.Kind
	default:
		err := pusher.Push(ctx, person, route, notice)
		var refused Refused
		switch {
		case err == nil:
			delivery.State, delivery.LastError, delivery.SentAt = DeliverySent, "", &now
		case errors.As(err, &refused):
			delivery.State, delivery.LastError = DeliveryFailed, err.Error()
		case !now.Before(delivery.GiveUpAt):
			delivery.State, delivery.LastError = DeliveryFailed, "gave up: "+err.Error()
		default:
			delivery.LastError = err.Error()
			delivery.NextAttemptAt = now.Add(backoff(delivery.Attempts))
		}
	}
	_ = d.store.UpdateDelivery(ctx, delivery)
	return delivery.State != DeliveryPending
}

func backoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}
