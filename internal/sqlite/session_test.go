package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

func sessionIDs(values ...string) service.IDGenerator {
	var mu sync.Mutex
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(values) == 0 {
			return "", errors.New("session ID sequence exhausted")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
}
func newSessionService(t *testing.T, path string, clock *mutableClock, ids service.IDGenerator) (*sqlite.Store, *service.Service) {
	t.Helper()
	persistence, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(persistence, clock, service.WithMaxLeaseDuration(time.Hour), service.WithTokenGenerator(tokenSequence("lease-a", "lease-b", "lease-c")), service.WithIDGenerator(ids))
	if err != nil {
		_ = persistence.Close()
		t.Fatal(err)
	}
	return persistence, svc
}
func adopt(t *testing.T, svc *service.Service, lease service.AdapterLease, key string) service.Session {
	t.Helper()
	value, err := svc.AdoptSession(context.Background(), service.AdoptSessionRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterKey: key, Label: "Work", Location: "local", Status: "idle", Capabilities: []string{"events", "events"}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func TestSessionsAreOpaqueToClientsAndAdoptionSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "sessions.db")
	persistence, svc := newSessionService(t, path, clock, sessionIDs("public-one", "ignored-retry", "public-two", "event-one", "event-two"))
	alpha := register(t, svc, "adapter.alpha", "alpha")
	first := adopt(t, svc, alpha, "opaque-native-one")
	encoded, err := json.Marshal(first)
	if err != nil || first.SessionID != "public-one" || strings.Contains(string(encoded), "opaque-native-one") || len(first.Capabilities) != 1 {
		t.Fatalf("public adoption = %#v", first)
	}
	retry := adopt(t, svc, alpha, "opaque-native-one")
	if retry.SessionID != first.SessionID || retry.Revision != 1 {
		t.Fatalf("adoption retry = %#v", retry)
	}
	second := adopt(t, svc, alpha, "opaque-native-two")
	if second.SessionID == first.SessionID {
		t.Fatalf("distinct adoption = %#v", second)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	persistence, restarted := newSessionService(t, path, clock, sessionIDs("ignored-after-restart"))
	defer persistence.Close()
	again := adopt(t, restarted, alpha, "opaque-native-one")
	if again.SessionID != first.SessionID {
		t.Fatalf("restart adoption = %#v", again)
	}
	listed, err := restarted.ListSessions(ctx, 10)
	listedJSON, marshalErr := json.Marshal(listed)
	if err != nil || marshalErr != nil || len(listed) != 2 || strings.Contains(string(listedJSON), "opaque-native") {
		t.Fatalf("public list = %#v, %v", listed, err)
	}
}
func TestSessionMutationAndEventsFenceRevisionLeaseAndOperationReuse(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newSessionService(t, filepath.Join(t.TempDir(), "events.db"), clock, sessionIDs("session", "event-1", "event-2", "event-3", "event-4", "event-5"))
	defer persistence.Close()
	alpha := register(t, svc, "adapter.alpha", "alpha")
	beta := register(t, svc, "adapter.beta", "beta")
	session := adopt(t, svc, alpha, "opaque")
	if _, err := svc.UpdateSession(ctx, service.UpdateSessionRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, SessionID: session.SessionID, ExpectedRevision: 1, Label: "bad", Status: "idle"}); !hasCode(err, service.CodeAdapterMismatch) {
		t.Fatalf("foreign update = %v", err)
	}
	if _, err := svc.UpdateSession(ctx, service.UpdateSessionRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: 2, Label: "bad", Status: "idle"}); !hasCode(err, service.CodeStaleRevision) {
		t.Fatalf("stale update = %v", err)
	}
	updated, err := svc.UpdateSession(ctx, service.UpdateSessionRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: 1, Label: "Updated", Status: "busy", Capabilities: []string{"events"}})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: 1, OperationID: "stale-event", EventType: "state", Payload: []byte(`{}`)}); !hasCode(err, service.CodeStaleRevision) {
		t.Fatalf("stale append = %v", err)
	}
	first, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: updated.Revision, OperationID: "operation-1", EventType: "state", Payload: []byte(`{"value":"one"}`)})
	if err != nil || first.Sequence != 1 || first.Cursor != 1 {
		t.Fatalf("first append = %#v, %v", first, err)
	}
	advanced, err := svc.UpdateSession(ctx, service.UpdateSessionRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: updated.Revision, Label: "Later", Status: "idle", Capabilities: []string{"events"}})
	if err != nil || advanced.Revision != 3 {
		t.Fatalf("advance session = %#v, %v", advanced, err)
	}
	replay, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "operation-1", EventType: "state", Payload: []byte(`{"value":"one"}`)})
	if err != nil || replay.EventID != first.EventID {
		t.Fatalf("replay after revision advance = %#v, %v", replay, err)
	}
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "operation-2", EventType: "state", Payload: []byte(`{}`)}); !hasCode(err, service.CodeAdapterMismatch) {
		t.Fatalf("foreign append = %v", err)
	}
	explicit := clock.Now().Add(-time.Minute)
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "explicit-time", EventType: "state", Payload: []byte(`{"value":"time"}`), OccurredAt: explicit}); err != nil {
		t.Fatalf("explicit event = %v", err)
	}
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "explicit-time", EventType: "state", Payload: []byte(`{"value":"time"}`), OccurredAt: explicit.Add(time.Second)}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("explicit time conflict = %v", err)
	}
	clock.Advance(11 * time.Minute)
	expiredReplay, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "operation-1", EventType: "state", Payload: []byte(`{"value":"one"}`)})
	if err != nil || expiredReplay.EventID != first.EventID {
		t.Fatalf("replay after lease expiry = %#v, %v", expiredReplay, err)
	}
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "operation-1", EventType: "state", Payload: []byte(`{"value":"other"}`)}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("conflict append = %v", err)
	}
	if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: advanced.Revision, OperationID: "operation-2", EventType: "state", Payload: []byte(`{}`)}); !hasCode(err, service.CodeLeaseExpired) {
		t.Fatalf("expired append = %v", err)
	}
}
func TestSessionEventCursorAndOrderPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "cursor.db")
	persistence, svc := newSessionService(t, path, clock, sessionIDs("session", "event-1", "event-2"))
	alpha := register(t, svc, "adapter.alpha", "alpha")
	session := adopt(t, svc, alpha, "opaque")
	for _, op := range []string{"one", "two"} {
		if _, err := svc.AppendSessionEvent(ctx, service.AppendSessionEventRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, SessionID: session.SessionID, ExpectedRevision: 1, OperationID: op, EventType: "state", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	persistence, restarted := newSessionService(t, path, clock, sessionIDs("unused"))
	defer persistence.Close()
	events, err := restarted.ListSessionEvents(ctx, session.SessionID, 0, 10)
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[0].Cursor >= events[1].Cursor {
		t.Fatalf("persisted events = %#v, %v", events, err)
	}
	resumed, err := restarted.ListSessionEvents(ctx, session.SessionID, events[0].Cursor, 10)
	if err != nil || len(resumed) != 1 || resumed[0].EventID != events[1].EventID {
		t.Fatalf("resumed events = %#v, %v", resumed, err)
	}
}
