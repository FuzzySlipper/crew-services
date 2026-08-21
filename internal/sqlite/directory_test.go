package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

type mutableClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *mutableClock) Advance(value time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(value)
}

func tokenSequence(values ...string) service.TokenGenerator {
	var mu sync.Mutex
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(values) == 0 {
			return "", errors.New("token sequence exhausted")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
}

func newService(t *testing.T, path string, clock *mutableClock, tokens service.TokenGenerator) (*sqlite.Store, *service.Service) {
	t.Helper()
	persistence, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	svc, err := service.New(persistence, clock, service.WithMaxLeaseDuration(time.Hour), service.WithTokenGenerator(tokens))
	if err != nil {
		_ = persistence.Close()
		t.Fatalf("New() error = %v", err)
	}
	return persistence, svc
}

func register(t *testing.T, svc *service.Service, adapter, instance string) service.AdapterLease {
	t.Helper()
	return registerWithPrevious(t, svc, adapter, instance, "")
}

func registerWithPrevious(t *testing.T, svc *service.Service, adapter, instance, previousLeaseToken string) service.AdapterLease {
	t.Helper()
	lease, err := svc.RegisterAdapter(context.Background(), service.RegisterAdapterRequest{AdapterID: adapter, InstanceID: instance, PreviousLeaseToken: previousLeaseToken, LeaseDuration: 10 * time.Minute})
	if err != nil {
		t.Fatalf("RegisterAdapter(%s, %s) error = %v", adapter, instance, err)
	}
	return lease
}

func TestDirectoryPersistsFencingAndBindingGenerationsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "directory.db")
	persistence, svc := newService(t, path, clock, tokenSequence("token-one", "ignored-retry", "token-two", "token-beta", "ignored-restart"))

	first := register(t, svc, "adapter.alpha", "process-1")
	clock.Advance(time.Minute)
	retry := register(t, svc, "adapter.alpha", "process-1")
	if retry.LeaseToken != first.LeaseToken || !retry.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("same instance retry = %#v; first = %#v", retry, first)
	}
	second := registerWithPrevious(t, svc, "adapter.alpha", "process-2", retry.LeaseToken)
	if second.LeaseToken == first.LeaseToken || second.InstanceID != "process-2" {
		t.Fatalf("new instance lease = %#v; first = %#v", second, first)
	}
	if _, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.alpha", InstanceID: "process-1", LeaseDuration: 10 * time.Minute}); !hasCode(err, service.CodeLeaseFenced) {
		t.Fatalf("stale instance re-registration error = %v, want lease_fenced", err)
	}

	created, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/alice", ActorAdapterID: "adapter.alpha", LeaseToken: second.LeaseToken, AdapterID: "adapter.alpha", TargetRef: "native:opaque/alice", Capabilities: []string{"inbox", "alpha", "inbox"}})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if created.Revision != 1 || created.Generation != 1 || len(created.Capabilities) != 2 || created.Capabilities[0] != "alpha" || created.Capabilities[1] != "inbox" {
		t.Fatalf("created binding = %#v", created)
	}
	if _, err := svc.RenewAdapter(ctx, service.RenewAdapterRequest{AdapterID: second.AdapterID, LeaseToken: second.LeaseToken, LeaseDuration: 10 * time.Minute}); err != nil {
		t.Fatalf("renew current adapter: %v", err)
	}
	if unchanged, err := svc.Resolve(ctx, created.Address); err != nil || unchanged.Revision != created.Revision || unchanged.Generation != created.Generation {
		t.Fatalf("binding changed after lease renewal: %#v, %v", unchanged, err)
	}
	beta := register(t, svc, "adapter.beta", "process-1")
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/alice", ActorAdapterID: "adapter.alpha", LeaseToken: first.LeaseToken, AdapterID: "adapter.alpha", TargetRef: "native:opaque/stale"}); !hasCode(err, service.CodeLeaseFenced) {
		t.Fatalf("stale instance put error = %v, want lease_fenced", err)
	}

	capabilities, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: created.Address, ActorAdapterID: "adapter.alpha", LeaseToken: second.LeaseToken, AdapterID: "adapter.alpha", TargetRef: created.TargetRef, Capabilities: []string{"beta", "alpha"}, ExpectedRevision: pointer(created.Revision)})
	if err != nil || capabilities.Revision != 2 || capabilities.Generation != 1 {
		t.Fatalf("capability update = %#v, %v", capabilities, err)
	}
	rebound, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: created.Address, ActorAdapterID: "adapter.alpha", LeaseToken: second.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "native:opaque/alice-next", Capabilities: capabilities.Capabilities, ExpectedRevision: pointer(capabilities.Revision)})
	if err != nil || rebound.Revision != 3 || rebound.Generation != 2 {
		t.Fatalf("semantic rebind = %#v, %v", rebound, err)
	}
	unbound, err := svc.Unbind(ctx, service.UnbindRequest{Address: created.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, ExpectedRevision: rebound.Revision})
	if err != nil || unbound.Bound || unbound.Revision != 4 || unbound.Generation != 3 {
		t.Fatalf("unbind = %#v, %v", unbound, err)
	}
	if tombstone, err := svc.Resolve(ctx, created.Address); err != nil || tombstone.Bound || tombstone.Revision != unbound.Revision || tombstone.Generation != unbound.Generation {
		t.Fatalf("resolve unbound = %#v, %v", tombstone, err)
	}
	if _, err := svc.FenceSender(ctx, service.SenderFenceRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, SenderAddress: created.Address}); !hasCode(err, service.CodeNotBound) {
		t.Fatalf("fence unbound sender error = %v, want not_bound", err)
	}
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: created.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "native:opaque/no-cas"}); !hasCode(err, service.CodeConflict) {
		t.Fatalf("create over tombstone error = %v, want conflict", err)
	}
	final, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: created.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "native:opaque/alice-final", Capabilities: []string{"alpha"}, ExpectedRevision: pointer(unbound.Revision)})
	if err != nil || final.Revision != 5 || final.Generation != 4 {
		t.Fatalf("tombstone rebind = %#v, %v", final, err)
	}
	if _, err := svc.FenceSender(ctx, service.SenderFenceRequest{AdapterID: "adapter.alpha", LeaseToken: second.LeaseToken, SenderAddress: created.Address}); !hasCode(err, service.CodeAdapterMismatch) {
		t.Fatalf("FenceSender mismatched adapter error = %v, want adapter_mismatch", err)
	}
	if _, err := svc.FenceSender(ctx, service.SenderFenceRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, SenderAddress: created.Address}); err != nil {
		t.Fatalf("FenceSender current binding: %v", err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	persistence, restarted := newService(t, path, clock, tokenSequence("ignored-after-restart"))
	defer persistence.Close()
	resolved, err := restarted.Resolve(ctx, created.Address)
	if err != nil || resolved.Generation != 4 || resolved.TargetRef != "native:opaque/alice-final" {
		t.Fatalf("resolve after restart = %#v, %v", resolved, err)
	}
	if _, err := restarted.FenceSender(ctx, service.SenderFenceRequest{AdapterID: "adapter.alpha", LeaseToken: first.LeaseToken, SenderAddress: created.Address}); !hasCode(err, service.CodeLeaseFenced) {
		t.Fatalf("old lease after restart error = %v, want lease_fenced", err)
	}
	if _, err := restarted.FenceSender(ctx, service.SenderFenceRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, SenderAddress: created.Address}); err != nil {
		t.Fatalf("current lease after restart: %v", err)
	}
}

func TestExpiredLeaseAndSameInstanceReregistration(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newService(t, filepath.Join(t.TempDir(), "expired.db"), clock, tokenSequence("durable-token", "unused", "after-expiry-token"))
	defer persistence.Close()
	lease := register(t, svc, "adapter.beta", "process-1")
	clock.Advance(11 * time.Minute)
	if _, err := svc.RenewAdapter(ctx, service.RenewAdapterRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, LeaseDuration: time.Minute}); !hasCode(err, service.CodeLeaseExpired) {
		t.Fatalf("expired renewal error = %v, want lease_expired", err)
	}
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/beta", ActorAdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterID: lease.AdapterID, TargetRef: "opaque"}); !hasCode(err, service.CodeLeaseExpired) {
		t.Fatalf("expired put error = %v, want lease_expired", err)
	}
	resumed := register(t, svc, "adapter.beta", "process-1")
	if resumed.LeaseToken != lease.LeaseToken {
		t.Fatalf("same instance re-registration token = %q, want %q", resumed.LeaseToken, lease.LeaseToken)
	}
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/beta", ActorAdapterID: resumed.AdapterID, LeaseToken: resumed.LeaseToken, AdapterID: resumed.AdapterID, TargetRef: "opaque"}); err != nil {
		t.Fatalf("put after re-registration: %v", err)
	}
	clock.Advance(11 * time.Minute)
	takeover := register(t, svc, "adapter.beta", "process-2")
	if takeover.LeaseToken == resumed.LeaseToken || takeover.InstanceID != "process-2" {
		t.Fatalf("expired different-instance takeover = %#v; resumed = %#v", takeover, resumed)
	}
}

func TestBindingCASRaceHasOneWinnerAndListIsStable(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newService(t, filepath.Join(t.TempDir(), "race.db"), clock, tokenSequence("race-token"))
	defer persistence.Close()
	lease := register(t, svc, "adapter.race", "process-race")
	created, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/zeta", ActorAdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterID: lease.AdapterID, TargetRef: "opaque/z"})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/alpha", ActorAdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterID: lease.AdapterID, TargetRef: "opaque/a"}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}

	results := make(chan error, 2)
	for _, capability := range []string{"one", "two"} {
		capability := capability
		go func() {
			_, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: created.Address, ActorAdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterID: lease.AdapterID, TargetRef: created.TargetRef, Capabilities: []string{capability}, ExpectedRevision: pointer(created.Revision)})
			results <- err
		}()
	}
	var successes, stale int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		if hasCode(err, service.CodeStaleRevision) {
			stale++
			continue
		}
		t.Fatalf("race error = %v", err)
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("CAS results: successes=%d stale=%d", successes, stale)
	}
	bindings, err := svc.List(ctx)
	if err != nil || len(bindings) != 2 || bindings[0].Address != "agent/alpha" || bindings[1].Address != "agent/zeta" {
		t.Fatalf("deterministic list = %#v, %v", bindings, err)
	}
}

func pointer(value int64) *int64 { return &value }

func hasCode(err error, want service.ErrorCode) bool {
	var typed *service.Error
	return errors.As(err, &typed) && typed.Code == want
}
