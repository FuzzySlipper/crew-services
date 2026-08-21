package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
	"crew-services/internal/store"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time          { return c.now }
func (c *mutableClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type testStore struct {
	store.Store
	err error
}

func (s testStore) Ready(context.Context) error { return s.err }
func (testStore) Close() error                  { return nil }

func TestHealthzReturnsJSONReadiness(t *testing.T) {
	t.Parallel()

	svc, err := service.New(testStore{}, fixedClock{now: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	NewHandler(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestReadyzReportsUnavailableStore(t *testing.T) {
	t.Parallel()

	svc, err := service.New(testStore{err: errors.New("down")}, fixedClock{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	NewHandler(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestDirectoryEndpointsUseTypedJSONErrorsAndCAS(t *testing.T) {
	persistence, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "directory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer persistence.Close()
	svc, err := service.New(persistence, fixedClock{now: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}, service.WithTokenGenerator(func() (string, error) { return "http-token", nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	handler := NewHandler(svc)
	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, httptest.NewRequest(http.MethodGet, "/v1/addresses", nil))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"addresses":[]`) {
		t.Fatalf("empty list = %d %s", empty.Code, empty.Body.String())
	}

	register := requestJSON(t, handler, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": "adapter.http", "instance_id": "process-http", "lease_duration": "1m"})
	if register.Code != http.StatusCreated || !strings.Contains(register.Body.String(), `"lease_token":"http-token"`) {
		t.Fatalf("register = %d %s", register.Code, register.Body.String())
	}
	created := requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Fhttp/binding", map[string]any{"actor_adapter_id": "adapter.http", "lease_token": "http-token", "adapter_id": "adapter.http", "target_ref": "opaque://http", "capabilities": []string{"z", "a", "z"}})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"capabilities":["a","z"]`) {
		t.Fatalf("create binding = %d %s", created.Code, created.Body.String())
	}
	resolved := httptest.NewRecorder()
	handler.ServeHTTP(resolved, httptest.NewRequest(http.MethodGet, "/v1/addresses/agent%2Fhttp", nil))
	if resolved.Code != http.StatusOK || !strings.Contains(resolved.Body.String(), `"target_ref":"opaque://http"`) {
		t.Fatalf("resolve = %d %s", resolved.Code, resolved.Body.String())
	}
	stale := requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Fhttp/binding", map[string]any{"actor_adapter_id": "adapter.http", "lease_token": "http-token", "adapter_id": "adapter.http", "target_ref": "opaque://http", "expected_revision": 99})
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"stale_revision"`) {
		t.Fatalf("stale update = %d %s", stale.Code, stale.Body.String())
	}
	unbound := requestJSON(t, handler, http.MethodDelete, "/v1/addresses/agent%2Fhttp/binding", map[string]any{"actor_adapter_id": "adapter.http", "lease_token": "http-token", "expected_revision": 1})
	if unbound.Code != http.StatusOK || !strings.Contains(unbound.Body.String(), `"bound":false`) {
		t.Fatalf("unbind = %d %s", unbound.Code, unbound.Body.String())
	}
	tombstone := httptest.NewRecorder()
	handler.ServeHTTP(tombstone, httptest.NewRequest(http.MethodGet, "/v1/addresses/agent%2Fhttp", nil))
	if tombstone.Code != http.StatusOK || !strings.Contains(tombstone.Body.String(), `"bound":false`) || !strings.Contains(tombstone.Body.String(), `"generation":2`) {
		t.Fatalf("resolve tombstone = %d %s", tombstone.Code, tombstone.Body.String())
	}
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v1/addresses", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"addresses"`) {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
}

func TestMessageEndpointsExposeReplayAndReadOnlyLedger(t *testing.T) {
	persistence, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer persistence.Close()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	ids := []string{"message-http", "delivery-http", "unused-message", "unused-delivery", "conflict-message", "conflict-delivery"}
	svc, err := service.New(persistence, fixedClock{now: now}, service.WithMaxLeaseDuration(time.Hour), service.WithMaxTTLDuration(time.Hour), service.WithTokenGenerator(func() (string, error) { return "http-token", nil }), service.WithIDGenerator(func() (string, error) { value := ids[0]; ids = ids[1:]; return value, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	handler := NewHandler(svc)
	requestJSON(t, handler, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": "adapter.sender", "instance_id": "sender", "lease_duration": "1m"})
	requestJSON(t, handler, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": "adapter.recipient", "instance_id": "recipient", "lease_duration": "1m"})
	requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Fsender/binding", map[string]any{"actor_adapter_id": "adapter.sender", "lease_token": "http-token", "adapter_id": "adapter.sender", "target_ref": "opaque:sender"})
	requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Frecipient/binding", map[string]any{"actor_adapter_id": "adapter.recipient", "lease_token": "http-token", "adapter_id": "adapter.recipient", "target_ref": "opaque:recipient"})
	body := map[string]any{"producer_id": "adapter.sender", "lease_token": "http-token", "operation_id": "http-operation", "sender_address": "agent/sender", "recipient_address": "agent/recipient", "body": "hello", "ttl": "1m"}
	created := requestJSON(t, handler, http.MethodPost, "/v1/messages", body)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"message_id":"message-http"`) || !strings.Contains(created.Body.String(), `"replayed":false`) {
		t.Fatalf("created message = %d %s", created.Code, created.Body.String())
	}
	replayed := requestJSON(t, handler, http.MethodPost, "/v1/messages", body)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"replayed":true`) {
		t.Fatalf("replay message = %d %s", replayed.Code, replayed.Body.String())
	}
	message := httptest.NewRecorder()
	handler.ServeHTTP(message, httptest.NewRequest(http.MethodGet, "/v1/messages/message-http", nil))
	if message.Code != http.StatusOK || !strings.Contains(message.Body.String(), `"body":"hello"`) {
		t.Fatalf("get message = %d %s", message.Code, message.Body.String())
	}
	mailbox := httptest.NewRecorder()
	handler.ServeHTTP(mailbox, httptest.NewRequest(http.MethodGet, "/v1/mailbox/agent%2Frecipient", nil))
	if mailbox.Code != http.StatusOK || !strings.Contains(mailbox.Body.String(), `"state":"queued"`) {
		t.Fatalf("mailbox = %d %s", mailbox.Code, mailbox.Body.String())
	}
	conflict := requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{"producer_id": "adapter.sender", "lease_token": "http-token", "operation_id": "http-operation", "sender_address": "agent/sender", "recipient_address": "agent/recipient", "body": "different", "ttl": "1m"})
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"operation_conflict"`) {
		t.Fatalf("operation conflict = %d %s", conflict.Code, conflict.Body.String())
	}
}

func TestRoundAndTrafficEndpointsRemainThinAndReadOnly(t *testing.T) {
	persistence, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "round-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	ids := []string{"missing-message", "missing-delivery", "round-http", "root-http", "root-delivery-http", "reply-http", "reply-delivery-http", "mismatch-message", "mismatch-delivery"}
	svc, err := service.New(persistence, fixedClock{now: now}, service.WithMaxLeaseDuration(time.Hour), service.WithMaxTTLDuration(time.Hour), service.WithTokenGenerator(func() (string, error) { return "http-token", nil }), service.WithIDGenerator(func() (string, error) { value := ids[0]; ids = ids[1:]; return value, nil }))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(svc)
	for _, adapter := range []string{"sender", "recipient"} {
		requestJSON(t, handler, http.MethodPost, "/v1/adapters/register", map[string]any{"adapter_id": "adapter." + adapter, "instance_id": adapter, "lease_duration": "1h"})
	}
	requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Fsender/binding", map[string]any{"actor_adapter_id": "adapter.sender", "lease_token": "http-token", "adapter_id": "adapter.sender", "target_ref": "sender"})
	requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Frecipient/binding", map[string]any{"actor_adapter_id": "adapter.recipient", "lease_token": "http-token", "adapter_id": "adapter.recipient", "target_ref": "recipient"})
	missing := requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{"producer_id": "adapter.recipient", "lease_token": "http-token", "operation_id": "missing", "sender_address": "agent/recipient", "recipient_address": "agent/sender", "body": "answer", "reply_to_message_id": "none", "ttl": "1h"})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"reply_original_not_found"`) {
		t.Fatalf("missing reply = %d %s", missing.Code, missing.Body.String())
	}
	root := map[string]any{"producer_id": "adapter.sender", "lease_token": "http-token", "operation_id": "root-op", "sender_address": "agent/sender", "recipient_address": "agent/recipient", "body": "question", "ttl": "1h", "round_ttl": "30m"}
	created := requestJSON(t, handler, http.MethodPost, "/v1/rounds", root)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"round_id":"round-http"`) {
		t.Fatalf("create round = %d %s", created.Code, created.Body.String())
	}
	reply := requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{"producer_id": "adapter.recipient", "lease_token": "http-token", "operation_id": "reply-op", "sender_address": "agent/recipient", "recipient_address": "agent/sender", "body": "answer", "reply_to_message_id": "root-http", "ttl": "1h"})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply = %d %s", reply.Code, reply.Body.String())
	}
	requestJSON(t, handler, http.MethodPut, "/v1/addresses/agent%2Frecipient/binding", map[string]any{"actor_adapter_id": "adapter.recipient", "lease_token": "http-token", "adapter_id": "adapter.recipient", "target_ref": "recipient-next", "expected_revision": 1})
	mismatch := requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{"producer_id": "adapter.recipient", "lease_token": "http-token", "operation_id": "mismatch", "sender_address": "agent/recipient", "recipient_address": "agent/sender", "body": "answer", "reply_to_message_id": "root-http", "ttl": "1h"})
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), `"code":"reply_generation_mismatch"`) {
		t.Fatalf("generation mismatch = %d %s", mismatch.Code, mismatch.Body.String())
	}
	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/v1/rounds/round-http", nil))
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"replied"`) {
		t.Fatalf("get round = %d %s", got.Code, got.Body.String())
	}
	traffic := httptest.NewRecorder()
	handler.ServeHTTP(traffic, httptest.NewRequest(http.MethodGet, "/v1/traffic", nil))
	if traffic.Code != http.StatusOK || !strings.Contains(traffic.Body.String(), `"messages":[`) || !strings.Contains(traffic.Body.String(), `"rounds":[`) {
		t.Fatalf("traffic = %d %s", traffic.Code, traffic.Body.String())
	}
}

func TestMaintenanceReapReleasesExpiredClaimAndIsRepeatable(t *testing.T) {
	ctx := context.Background()
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "reap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}
	tokens := []string{"alpha", "beta", "claim"}
	svc, err := service.New(persistence, clock, service.WithMaxLeaseDuration(time.Hour), service.WithMaxTTLDuration(time.Hour), service.WithTokenGenerator(func() (string, error) { value := tokens[0]; tokens = tokens[1:]; return value, nil }), service.WithIDGenerator(func() (string, error) { return "id", nil }))
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.alpha", InstanceID: "alpha", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.beta", InstanceID: "beta", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/alice", ActorAdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, AdapterID: alpha.AdapterID, TargetRef: "alice"}); err != nil {
		t.Fatal(err)
	}
	bob, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "bob", Capabilities: []string{"deliver_when_idle"}})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "send", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "hello", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	handler := NewHandler(svc)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/maintenance/reap", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"deliveries":[`) || !strings.Contains(first.Body.String(), `"state":"queued"`) {
		t.Fatalf("first reap=%d %s", first.Code, first.Body.String())
	}
	if value, err := svc.GetDelivery(ctx, submitted.Delivery.DeliveryID); err != nil || value.State != "queued" {
		t.Fatalf("reaped delivery=%#v %v", value, err)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/maintenance/reap", nil))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"deliveries":[]`) {
		t.Fatalf("second reap=%d %s", second.Code, second.Body.String())
	}
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(encoded)))
	return recorder
}
