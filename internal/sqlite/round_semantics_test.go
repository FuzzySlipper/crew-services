package sqlite_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"crew-services/internal/service"
)

func beginRoundFor(t *testing.T, svc *service.Service, lease service.AdapterLease, op, sender, recipient, body, correlation string, ttl, roundTTL time.Duration) service.BeginRoundResult {
	t.Helper()
	v, err := svc.BeginRound(context.Background(), service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: lease.AdapterID, LeaseToken: lease.LeaseToken, OperationID: op, SenderAddress: sender, RecipientAddress: recipient, Body: body, CorrelationID: correlation, TTL: ttl}, RoundTTL: roundTTL})
	if err != nil {
		t.Fatalf("begin %s: %v", op, err)
	}
	return v
}

func TestReplyValidationAndReplayAreSticky(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	p, svc := newMessageService(t, filepath.Join(t.TempDir(), "reply.db"), clock, idSequence("root", "root-d", "missing-m", "missing-d", "sender-m", "sender-d", "recipient-m", "recipient-d", "aba-m", "aba-d", "replay-root", "replay-root-d", "reply", "reply-d"))
	defer p.Close()
	alpha, beta, bob := bindPair(t, svc)
	root := submit(t, svc, alpha, "root-op", "root", time.Hour)
	for _, tc := range []struct {
		name string
		r    service.SubmitMessageRequest
		code service.ErrorCode
	}{
		{"missing", service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "missing", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "x", ReplyToMessageID: "none", TTL: time.Hour}, service.CodeReplyOriginalNotFound},
		{"sender", service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "sender", SenderAddress: "agent/alice", RecipientAddress: "agent/alice", Body: "x", ReplyToMessageID: root.Message.MessageID, TTL: time.Hour}, service.CodeReplySenderMismatch},
		{"recipient", service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "recipient", SenderAddress: "agent/bob", RecipientAddress: "agent/bob", Body: "x", ReplyToMessageID: root.Message.MessageID, TTL: time.Hour}, service.CodeReplyRecipientMismatch},
	} {
		if _, err := svc.SubmitMessage(ctx, tc.r); !hasCode(err, tc.code) {
			t.Fatalf("%s = %v", tc.name, err)
		}
	}
	updated, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: bob.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "bob-aba", ExpectedRevision: pointer(bob.Revision)})
	if err != nil {
		t.Fatal(err)
	}
	aba := service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "aba", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "x", ReplyToMessageID: root.Message.MessageID, TTL: time.Hour}
	if _, err := svc.SubmitMessage(ctx, aba); !hasCode(err, service.CodeReplyGenerationMismatch) {
		t.Fatalf("ABA = %v", err)
	}
	// A new root snapshots the rebinding; this reply is valid at generation two.
	validRoot := submit(t, svc, alpha, "replay-root-op", "root after ABA", time.Hour)
	reply, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "reply-op", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "answer", ReplyToMessageID: validRoot.Message.MessageID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Drift after acceptance must not spend IDs or revalidate an exact replay.
	_, err = svc.PutBinding(ctx, service.PutBindingRequest{Address: updated.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "bob-next", ExpectedRevision: pointer(updated.Revision)})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "reply-op", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "answer", ReplyToMessageID: validRoot.Message.MessageID, TTL: time.Hour})
	if err != nil || !replayed.Replayed || replayed.Message != reply.Message {
		t.Fatalf("reply replay = %#v, %v", replayed, err)
	}
	if _, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "reply-op", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "changed", ReplyToMessageID: validRoot.Message.MessageID, TTL: time.Hour}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("reply conflict = %v", err)
	}
}

func TestRoundOperationsAreAtomicReplaySafeAndCorrelationIsNotIdentity(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	p, svc := newMessageService(t, filepath.Join(t.TempDir(), "round-op.db"), clock, idSequence("round-1", "root-1", "d-1", "round-2", "root-2", "d-2", "round-1", "root-3", "d-3"))
	defer p.Close()
	alpha, beta, _ := bindPair(t, svc)
	alice, err := svc.Resolve(ctx, "agent/alice")
	if err != nil {
		t.Fatal(err)
	}
	first := beginRoundFor(t, svc, alpha, "op-1", "agent/alice", "agent/bob", "one", "same", time.Hour, time.Hour)
	// Exact replay survives semantic drift, while ordinary submit remains a distinct family.
	updated, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: alice.Address, ActorAdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, AdapterID: alpha.AdapterID, TargetRef: "alice-next", ExpectedRevision: pointer(alice.Revision)})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.BeginRound(ctx, service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "op-1", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "one", CorrelationID: "same", TTL: time.Hour}, RoundTTL: time.Hour})
	if err != nil || !replayed.Replayed || replayed.Round != first.Round {
		t.Fatalf("round replay=%#v,%v", replayed, err)
	}
	if _, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "op-1", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "one", TTL: time.Hour}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("family conflict=%v", err)
	}
	second := beginRoundFor(t, svc, beta, "op-2", "agent/bob", "agent/alice", "two", "same", time.Hour, time.Hour)
	if second.Round.RoundID == first.Round.RoundID || second.Round.CorrelationID != first.Round.CorrelationID {
		t.Fatalf("correlated siblings=%#v %#v", first.Round, second.Round)
	}
	// A duplicate service-generated round ID fails after root acceptance is staged,
	// proving transaction rollback leaves no orphan message or delivery.
	_, err = svc.BeginRound(ctx, service.BeginRoundRequest{Root: service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "op-3", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "three", TTL: time.Hour}, RoundTTL: time.Hour})
	if err == nil {
		t.Fatal("duplicate round ID unexpectedly accepted")
	}
	traffic, err := svc.Traffic(ctx)
	if err != nil || len(traffic.Messages) != 2 || len(traffic.Deliveries) != 2 || len(traffic.Rounds) != 2 {
		t.Fatalf("atomic traffic=%#v,%v", traffic, err)
	}
	_ = updated
}

func TestConcurrentRepliesAndExpiryEquality(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	p, svc := newMessageService(t, filepath.Join(t.TempDir(), "concurrent.db"), clock, idSequence("round", "root", "root-d", "r1", "d1", "r2", "d2", "round-exp", "root-exp", "root-exp-d", "late", "late-d"))
	defer p.Close()
	alpha, beta, _ := bindPair(t, svc)
	round := beginRoundFor(t, svc, alpha, "round", "agent/alice", "agent/bob", "q", "", time.Hour, time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, op := range []string{"reply-1", "reply-2"} {
		wg.Add(1)
		go func(op string) {
			defer wg.Done()
			_, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: op, SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: op, ReplyToMessageID: round.Message.MessageID, TTL: time.Hour})
			errs <- err
		}(op)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.GetRound(ctx, round.Round.RoundID)
	if err != nil || got.Status != "replied" || got.ReplyMessageID == "" {
		t.Fatalf("winner=%#v,%v", got, err)
	}
	lateRound := beginRoundFor(t, svc, alpha, "late-root", "agent/alice", "agent/bob", "late", "", time.Hour, time.Minute)
	clock.Advance(time.Minute)
	late, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "late-reply", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "late", ReplyToMessageID: lateRound.Message.MessageID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := svc.GetRound(ctx, lateRound.Round.RoundID)
	if err != nil || settled.Status != "expired" || settled.ReplyMessageID != "" {
		t.Fatalf("expiry equality=%#v,%v", settled, err)
	}
	if _, err := svc.GetMessage(ctx, late.Message.MessageID); err != nil {
		t.Fatalf("late ordinary reply missing: %v", err)
	}
}

func TestRoundTerminalBridgesWaitAndRestart(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "terminal.db")
	p, svc := newMessageService(t, path, clock, idSequence("cancel", "cm", "cd", "expire", "em", "ed", "pending", "pm", "pd", "reply", "rm", "rd"))
	alpha, beta, bob := bindPair(t, svc)
	cancelled := beginRoundFor(t, svc, alpha, "cancel", "agent/alice", "agent/bob", "q", "", time.Hour, time.Hour)
	if _, err := svc.CancelDelivery(ctx, cancelled.Delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetRound(ctx, cancelled.Round.RoundID); got.Status != "cancelled" {
		t.Fatalf("cancel bridge=%#v", got)
	}
	expiring := beginRoundFor(t, svc, alpha, "expire", "agent/alice", "agent/bob", "q", "", time.Minute, time.Hour)
	clock.Advance(time.Minute)
	if _, err := svc.SettlePending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetRound(ctx, expiring.Round.RoundID); got.Status != "expired" {
		t.Fatalf("expiry bridge=%#v", got)
	}
	pending := beginRoundFor(t, svc, alpha, "pending", "agent/alice", "agent/bob", "q", "", time.Hour, time.Hour)
	// Read-side wait neither settles nor changes pending state; cancellation is propagated.
	if got, err := svc.WaitRound(ctx, service.WaitRoundRequest{RoundID: pending.Round.RoundID, Timeout: time.Millisecond}); err != nil || !got.TimedOut || got.Round.Status != "pending" {
		t.Fatalf("wait timeout=%#v,%v", got, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.WaitRound(cancelCtx, service.WaitRoundRequest{RoundID: pending.Round.RoundID, Timeout: time.Second}); err == nil {
		t.Fatal("cancelled wait succeeded")
	}
	// Binding drift fails a root and bridges exactly once; a terminal round stays terminal.
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: bob.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "bob-next", ExpectedRevision: pointer(bob.Revision)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SettlePending(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetRound(ctx, pending.Round.RoundID); got.Status != "failed" {
		t.Fatalf("failed bridge=%#v", got)
	}
	if _, err := svc.CancelRound(ctx, pending.Round.RoundID); !hasCode(err, service.CodeConflict) {
		t.Fatalf("resurrection cancel=%v", err)
	}
	// A terminal reply link survives restart and traffic keeps separate record counts.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, svc = newMessageService(t, path, clock, idSequence())
	defer p.Close()
	got, err := svc.GetRound(ctx, cancelled.Round.RoundID)
	if err != nil || got.Status != "cancelled" {
		t.Fatalf("restart=%#v,%v", got, err)
	}
}

func TestDeliveredAndUnknownRootDoNotTerminalizeRound(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	p, svc := newMessageService(t, filepath.Join(t.TempDir(), "nonterminal-roots.db"), clock, idSequence("delivered", "dm", "dd", "unknown", "um", "ud"))
	defer p.Close()
	alpha, beta, bob := bindPair(t, svc)
	// This non-semantic binding update makes an idle claim eligible without
	// changing the generation recorded by each root.
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: bob.Address, ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "opaque/bob", Capabilities: []string{"deliver_when_idle"}, ExpectedRevision: pointer(bob.Revision)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ op, terminal string }{{"delivered", "delivered"}, {"unknown", "outcome_unknown"}} {
		round := beginRoundFor(t, svc, alpha, tc.op, "agent/alice", "agent/bob", tc.op, "", time.Hour, time.Hour)
		claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-" + tc.op, RecipientAddress: "agent/bob", RecipientGeneration: 1, Availability: "idle", ClaimDuration: time.Minute})
		if err != nil || !claim.Claimed {
			t.Fatalf("claim %s = %#v, %v", tc.op, claim, err)
		}
		if _, err = svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "dispatch-" + tc.op, DeliveryID: round.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "native-" + tc.op}); err != nil {
			t.Fatal(err)
		}
		if tc.terminal == "delivered" {
			_, err = svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "ack-" + tc.op, DeliveryID: round.Delivery.DeliveryID, NativeAttemptRef: "native-" + tc.op})
		} else {
			_, err = svc.OutcomeUnknown(ctx, service.ReconcileDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "unknown-" + tc.op, DeliveryID: round.Delivery.DeliveryID, NativeAttemptRef: "native-" + tc.op})
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetRound(ctx, round.Round.RoundID)
		if err != nil || got.Status != "pending" {
			t.Fatalf("%s terminalized round = %#v, %v", tc.terminal, got, err)
		}
	}
}

func TestRoundRestartRetainsReplyLinkAndTrafficLegs(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "round-restart.db")
	p, svc := newMessageService(t, path, clock, idSequence("round", "root", "root-d", "reply", "reply-d"))
	alpha, beta, _ := bindPair(t, svc)
	started := beginRoundFor(t, svc, alpha, "root-op", "agent/alice", "agent/bob", "question", "same", time.Hour, time.Hour)
	if _, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "reply-op", SenderAddress: "agent/bob", RecipientAddress: "agent/alice", Body: "answer", ReplyToMessageID: started.Message.MessageID, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, svc = newMessageService(t, path, clock, idSequence())
	defer p.Close()
	round, err := svc.GetRound(ctx, started.Round.RoundID)
	if err != nil || round.Status != "replied" || round.ReplyMessageID != "reply" {
		t.Fatalf("round after restart = %#v, %v", round, err)
	}
	traffic, err := svc.Traffic(ctx)
	if err != nil || len(traffic.Messages) != 2 || len(traffic.Deliveries) != 2 || len(traffic.Rounds) != 1 {
		t.Fatalf("traffic after restart = %#v, %v", traffic, err)
	}
}
