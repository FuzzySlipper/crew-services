package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
	_ "modernc.org/sqlite"
)

func idSequence(values ...string) service.IDGenerator {
	var mu sync.Mutex
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(values) == 0 {
			return "", errors.New("ID sequence exhausted")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
}

func newMessageService(t *testing.T, path string, clock *mutableClock, ids service.IDGenerator) (*sqlite.Store, *service.Service) {
	t.Helper()
	persistence, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	svc, err := service.New(persistence, clock, service.WithMaxLeaseDuration(time.Hour), service.WithMaxTTLDuration(time.Hour), service.WithTokenGenerator(tokenSequence("alpha-token", "beta-token", "replacement-token", "claim-token-2", "claim-token-3")), service.WithIDGenerator(ids))
	if err != nil {
		_ = persistence.Close()
		t.Fatalf("New() error = %v", err)
	}
	return persistence, svc
}

func bindPair(t *testing.T, svc *service.Service) (service.AdapterLease, service.AdapterLease, service.Binding) {
	t.Helper()
	ctx := context.Background()
	alpha := register(t, svc, "adapter.alpha", "alpha-instance")
	beta := register(t, svc, "adapter.beta", "beta-instance")
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/alice", ActorAdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, AdapterID: alpha.AdapterID, TargetRef: "opaque/alice"}); err != nil {
		t.Fatalf("bind alice: %v", err)
	}
	bob, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "opaque/bob"})
	if err != nil {
		t.Fatalf("bind bob: %v", err)
	}
	return alpha, beta, bob
}

func submit(t *testing.T, svc *service.Service, alpha service.AdapterLease, operation, body string, ttl time.Duration) service.SubmitMessageResult {
	t.Helper()
	result, err := svc.SubmitMessage(context.Background(), service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: operation, SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: body, TTL: ttl})
	if err != nil {
		t.Fatalf("SubmitMessage(%s): %v", operation, err)
	}
	return result
}

func TestMessageAcceptanceReplaySettlementAndRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "messages.db")
	persistence, svc := newMessageService(t, path, clock, idSequence("message-1", "delivery-1", "unused-retry-message", "unused-retry-delivery", "message-2", "delivery-2", "message-3", "delivery-3"))
	alpha, beta, bob := bindPair(t, svc)
	first := submit(t, svc, alpha, "operation-1", "hello", time.Minute)
	if first.Replayed || first.Message.MessageID != "message-1" || first.Delivery.DeliveryID != "delivery-1" || first.Delivery.State != "queued" || first.Message.SenderGeneration != 1 || first.Message.RecipientGeneration != 1 {
		t.Fatalf("first submit = %#v", first)
	}

	// A semantic recipient rebind changes new acceptance but does not alter the
	// exact retry result or require the original sender lease to remain valid.
	rebound, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: bob.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "opaque/bob-next", ExpectedRevision: pointer(bob.Revision)})
	if err != nil || rebound.Generation != 2 {
		t.Fatalf("rebind bob = %#v, %v", rebound, err)
	}
	clock.Advance(11 * time.Minute)
	replay, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "operation-1", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "hello", TTL: time.Minute})
	if err != nil || !replay.Replayed || replay.Message != first.Message || replay.Delivery != first.Delivery {
		t.Fatalf("replay = %#v, %v; first = %#v", replay, err, first)
	}
	if _, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "operation-1", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "different", TTL: time.Minute}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("operation conflict = %v", err)
	}

	// Reads after TTL and binding drift are observational: they expose queued
	// state until the explicit settlement operation runs.
	before, err := svc.GetDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || before.State != "queued" {
		t.Fatalf("read before settlement = %#v, %v", before, err)
	}
	if mailbox, err := svc.Mailbox(ctx, "agent/bob"); err != nil || len(mailbox) != 1 || mailbox[0] != before {
		t.Fatalf("mailbox mutated delivery = %#v, %v", mailbox, err)
	}
	settled, err := svc.SettlePending(ctx)
	if err != nil || len(settled) != 1 || settled[0].State != "expired" || settled[0].TerminalReason != "ttl_expired" {
		t.Fatalf("settlement = %#v, %v", settled, err)
	}
	if again, err := svc.SettlePending(ctx); err != nil || len(again) != 0 {
		t.Fatalf("repeat settlement = %#v, %v", again, err)
	}
	if _, err := svc.CancelDelivery(ctx, first.Delivery.DeliveryID); !hasCode(err, service.CodeConflict) {
		t.Fatalf("cancel terminal delivery = %v", err)
	}

	if err := persistence.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	persistence, restarted := newMessageService(t, path, clock, idSequence("after-restart-message", "after-restart-delivery"))
	defer persistence.Close()
	message, err := restarted.GetMessage(ctx, first.Message.MessageID)
	if err != nil || message != first.Message {
		t.Fatalf("message after restart = %#v, %v", message, err)
	}
	delivery, err := restarted.GetDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || delivery.State != "expired" || delivery.TerminalReason != "ttl_expired" {
		t.Fatalf("delivery after restart = %#v, %v", delivery, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open direct db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE messages SET body = 'mutated' WHERE message_id = ?`, first.Message.MessageID); err == nil {
		t.Fatal("immutable message update unexpectedly succeeded")
	}
	if _, err := db.Exec(`UPDATE deliveries SET state = 'queued', terminal_reason = '', terminal_at = NULL WHERE delivery_id = ?`, first.Delivery.DeliveryID); err == nil {
		t.Fatal("terminal delivery resurrection unexpectedly succeeded")
	}
}

func TestExactReplayAfterAuthorityDriftDoesNotNeedNewIDs(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "replay-no-ids.db"), clock, idSequence("message-only", "delivery-only"))
	defer persistence.Close()
	alpha, beta, bob := bindPair(t, svc)
	first := submit(t, svc, alpha, "replay-operation", "hello", time.Hour)
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: bob.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "opaque/bob-rebound", ExpectedRevision: pointer(bob.Revision)}); err != nil {
		t.Fatalf("rebind recipient: %v", err)
	}
	clock.Advance(11 * time.Minute) // the original producer lease has expired.
	replay, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "replay-operation", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "hello", TTL: time.Hour})
	if err != nil || !replay.Replayed || replay.Message != first.Message || replay.Delivery != first.Delivery {
		t.Fatalf("replay without IDs = %#v, %v; first=%#v", replay, err, first)
	}
	deliveries, err := svc.ListDeliveries(ctx)
	if err != nil || len(deliveries) != 1 || deliveries[0].DeliveryID != first.Delivery.DeliveryID {
		t.Fatalf("replay created another delivery = %#v, %v", deliveries, err)
	}
}

func TestDeliveryFIFOCancellationAndBindingDriftSettlement(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "fifo.db"), clock, idSequence("m1", "d1", "m2", "d2", "m3", "d3", "m4", "d4"))
	defer persistence.Close()
	alpha, beta, bob := bindPair(t, svc)
	first := submit(t, svc, alpha, "op-1", "one", time.Hour)
	second := submit(t, svc, alpha, "op-2", "two", time.Hour)
	if first.Delivery.AcceptedSequence >= second.Delivery.AcceptedSequence {
		t.Fatalf("sequence order: first=%d second=%d", first.Delivery.AcceptedSequence, second.Delivery.AcceptedSequence)
	}
	head, err := svc.HeadDelivery(ctx, "agent/bob", bob.Generation)
	if err != nil || head == nil || head.DeliveryID != first.Delivery.DeliveryID {
		t.Fatalf("first head = %#v, %v", head, err)
	}
	cancelled, err := svc.CancelDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || cancelled.State != "cancelled" || cancelled.TerminalReason != "cancelled_by_request" {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	repeat, err := svc.CancelDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || repeat.State != cancelled.State || repeat.TerminalReason != cancelled.TerminalReason {
		t.Fatalf("repeat cancel = %#v, %v", repeat, err)
	}
	head, err = svc.HeadDelivery(ctx, "agent/bob", bob.Generation)
	if err != nil || head == nil || head.DeliveryID != second.Delivery.DeliveryID {
		t.Fatalf("head after terminalization = %#v, %v", head, err)
	}

	third := submit(t, svc, alpha, "op-3", "three", time.Hour)
	rebound, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "opaque/bob-next", ExpectedRevision: pointer(bob.Revision)})
	if err != nil || rebound.Generation != 2 {
		t.Fatalf("rebind = %#v, %v", rebound, err)
	}
	settled, err := svc.SettlePending(ctx)
	if err != nil || len(settled) != 2 {
		t.Fatalf("drift settlement = %#v, %v", settled, err)
	}
	for _, delivery := range settled {
		if delivery.State != "failed" || delivery.TerminalReason != "binding_generation_changed" {
			t.Fatalf("drift settled delivery = %#v", delivery)
		}
	}
	if third.Delivery.AcceptedSequence <= second.Delivery.AcceptedSequence {
		t.Fatalf("third sequence = %d", third.Delivery.AcceptedSequence)
	}
	if head, err := svc.HeadDelivery(ctx, "agent/bob", bob.Generation); err != nil || head != nil {
		t.Fatalf("terminal mailbox head = %#v, %v", head, err)
	}

	fourth := submit(t, svc, alpha, "op-4", "four", time.Hour)
	if _, err := svc.Unbind(ctx, service.UnbindRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, ExpectedRevision: rebound.Revision}); err != nil {
		t.Fatalf("unbind bob: %v", err)
	}
	settled, err = svc.SettlePending(ctx)
	if err != nil || len(settled) != 1 || settled[0].DeliveryID != fourth.Delivery.DeliveryID || settled[0].TerminalReason != "recipient_unbound" {
		t.Fatalf("unbound settlement = %#v, %v", settled, err)
	}
}

func TestFailedValidationAndMissingReplyAreAtomic(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "atomic.db"), clock, idSequence("bad-message", "bad-delivery", "reply-message", "reply-delivery"))
	defer persistence.Close()
	alpha, _, _ := bindPair(t, svc)
	_, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "bad-sender", SenderAddress: "agent/not-alice", RecipientAddress: "agent/bob", Body: "hello", TTL: time.Minute})
	if !hasCode(err, service.CodeNotFound) {
		t.Fatalf("bad sender = %v", err)
	}
	_, err = svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "missing-reply", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "hello", ReplyToMessageID: "missing", TTL: time.Minute})
	if !hasCode(err, service.CodeReplyOriginalNotFound) {
		t.Fatalf("missing reply = %v", err)
	}
	if messages, err := svc.ListMessages(ctx); err != nil || len(messages) != 0 {
		t.Fatalf("failed submit persisted messages = %#v, %v", messages, err)
	}
	if deliveries, err := svc.ListDeliveries(ctx); err != nil || len(deliveries) != 0 {
		t.Fatalf("failed submit persisted deliveries = %#v, %v", deliveries, err)
	}
}
