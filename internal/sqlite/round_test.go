package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/service"
)

func TestRoundFirstReverseReplyWinsWhileSiblingsRemainMessages(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "round.db"), clock, idSequence("round-1", "root-1", "root-delivery", "reply-1", "reply-delivery-1", "reply-2", "reply-delivery-2"))
	defer persistence.Close()
	alpha, beta, _ := bindPair(t, svc)
	started, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "root-op", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "question", CorrelationID: "shared", TTL: time.Hour}, RoundTTL: 10 * time.Minute})
	if err != nil || started.Round.Status != "pending" || started.Message.MessageID != "root-1" {
		t.Fatalf("begin round = %#v, %v", started, err)
	}
	for _, op := range []string{"reply-op-1", "reply-op-2"} {
		if _, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: op, SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: op, ReplyToMessageID: started.Message.MessageID, TTL: time.Minute}); err != nil {
			t.Fatalf("submit %s: %v", op, err)
		}
	}
	round, err := svc.GetRound(ctx, started.Round.RoundID)
	if err != nil || round.Status != "replied" || round.ReplyMessageID != "reply-1" {
		t.Fatalf("resolved round = %#v, %v", round, err)
	}
	traffic, err := svc.Traffic(ctx)
	if err != nil || len(traffic.Messages) != 3 || len(traffic.Deliveries) != 3 || len(traffic.Rounds) != 1 {
		t.Fatalf("traffic = %#v, %v", traffic, err)
	}
}

func TestRoundExpiryWaitAndRootCancellationBridge(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "round-expiry.db"), clock, idSequence("round-expiry", "root-expiry", "delivery-expiry", "round-cancel", "root-cancel", "delivery-cancel"))
	defer persistence.Close()
	alpha, _, _ := bindPair(t, svc)
	expiring, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "expiry-op", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "question", TTL: time.Hour}, RoundTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	waited, err := svc.WaitRound(ctx, service.WaitRoundRequest{RoundID: expiring.Round.RoundID, Timeout: time.Millisecond})
	if err != nil || !waited.ObservedExpired || waited.Round.Status != "pending" {
		t.Fatalf("observed expiry = %#v, %v", waited, err)
	}
	settled, err := svc.SettleRounds(ctx)
	if err != nil || len(settled) != 1 || settled[0].Status != "expired" {
		t.Fatalf("settle round = %#v, %v", settled, err)
	}
	cancelled, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "cancel-op", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "question", TTL: time.Hour}, RoundTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelDelivery(ctx, cancelled.Delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetRound(ctx, cancelled.Round.RoundID)
	if err != nil || got.Status != "cancelled" || got.TerminalReason != "cancelled_by_request" {
		t.Fatalf("cancel bridge = %#v, %v", got, err)
	}
}
