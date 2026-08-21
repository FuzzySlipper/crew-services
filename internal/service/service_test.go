package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"crew-services/internal/store"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type memoryStore struct {
	store.Store
	err error
}

func (s memoryStore) Ready(context.Context) error { return s.err }
func (memoryStore) Close() error                  { return nil }

func TestReadyUsesInjectedClockAfterStoreCheck(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.FixedZone("PDT", -7*60*60))
	svc, err := New(memoryStore{}, fixedClock{now: now})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	status, err := svc.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if status.Status != "ok" || !status.Time.Equal(now.UTC()) {
		t.Fatalf("Ready() status = %#v", status)
	}
}

func TestReadyPropagatesStoreFailure(t *testing.T) {
	t.Parallel()

	svc, err := New(memoryStore{err: errors.New("unavailable")}, fixedClock{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := svc.Ready(context.Background()); err == nil {
		t.Fatal("Ready() error = nil, want store error")
	}
}
