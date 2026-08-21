package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"crew-services/internal/service"
)

func deliveryFixture(t *testing.T) (*mutableClock, *service.Service, func(), service.AdapterLease, service.AdapterLease, service.Binding) {
	t.Helper()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newMessageService(t, filepath.Join(t.TempDir(), "delivery.db"), clock, idSequence("m1", "d1", "m2", "d2", "m3", "d3"))
	alpha := register(t, svc, "adapter.alpha", "sender-1")
	beta := register(t, svc, "adapter.beta", "recipient-1")
	if _, err := svc.PutBinding(context.Background(), service.PutBindingRequest{Address: "agent/alice", ActorAdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, AdapterID: alpha.AdapterID, TargetRef: "sender"}); err != nil {
		t.Fatal(err)
	}
	bob, err := svc.PutBinding(context.Background(), service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: "recipient", Capabilities: []string{"deliver_when_idle", "durable_next_turn", "wake_inactive", "pending_notification"}})
	if err != nil {
		t.Fatal(err)
	}
	return clock, svc, func() { _ = persistence.Close() }, alpha, beta, bob
}

func TestFencedDeliveryLifecycleReplayFIFOAndUnknownSettlement(t *testing.T) {
	ctx := context.Background()
	clock, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first := submit(t, svc, alpha, "send-1", "first", time.Hour)
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-1", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "busy", ClaimDuration: time.Minute})
	if err != nil || !claim.Claimed || claim.DispatchAction != "register_next_turn" || claim.ClaimToken == "" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if raw, _ := json.Marshal(claim.Delivery); string(raw) != "" && string(raw) != "null" && contains(string(raw), "claim_token") {
		t.Fatalf("observational delivery exposed claim token: %s", raw)
	}
	replay, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: "stale-token", OperationID: "claim-1", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "busy", ClaimDuration: time.Minute})
	if err != nil || !replay.Replayed || replay.ClaimToken != claim.ClaimToken {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-1", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute}); !hasCode(err, service.CodeOperationConflict) {
		t.Fatalf("changed claim reuse=%v", err)
	}
	if _, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin-1", DeliveryID: first.Delivery.DeliveryID, ClaimToken: "wrong", NativeAttemptRef: "native-1"}); !hasCode(err, service.CodeClaimFenced) {
		t.Fatalf("wrong token=%v", err)
	}
	dispatching, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin-1", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "native-1"})
	if err != nil || dispatching.State != "dispatching" {
		t.Fatalf("begin=%#v %v", dispatching, err)
	}
	second := submit(t, svc, alpha, "send-2", "second", time.Hour)
	blocked, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-blocked", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil || blocked.Claimed || blocked.Reason != "head_dispatching" {
		t.Fatalf("fifo block=%#v %v", blocked, err)
	}
	delivered, err := svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "ack-1", DeliveryID: first.Delivery.DeliveryID, NativeAttemptRef: "native-1"})
	if err != nil || delivered.State != "delivered" {
		t.Fatalf("ack=%#v %v", delivered, err)
	}
	secondClaim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-2", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil || !secondClaim.Claimed || secondClaim.Delivery.DeliveryID != second.Delivery.DeliveryID {
		t.Fatalf("second=%#v %v", secondClaim, err)
	}
	if _, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin-2", DeliveryID: second.Delivery.DeliveryID, ClaimToken: secondClaim.ClaimToken, NativeAttemptRef: "native-2"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	settled, err := svc.SettlePending(ctx)
	if err != nil || len(settled) != 0 {
		t.Fatalf("live adapter dispatch settlement=%#v %v", settled, err)
	}
	clock.Advance(9 * time.Minute)
	settled, err = svc.SettlePending(ctx)
	if err != nil || len(settled) != 1 || settled[0].State != "outcome_unknown" {
		t.Fatalf("lost dispatch settlement=%#v %v", settled, err)
	}
	if persisted, err := svc.GetDelivery(ctx, second.Delivery.DeliveryID); err != nil || persisted.State != "outcome_unknown" {
		t.Fatalf("lost dispatch persisted=%#v %v", persisted, err)
	}
}

func TestClaimEligibilityAndLeaseEquality(t *testing.T) {
	ctx := context.Background()
	clock, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "send-1", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "first", ActivationPolicy: "never_wake", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	refused, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "never-busy", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "busy", ClaimDuration: time.Minute})
	if err != nil || refused.Claimed || refused.Reason != "not_eligible" {
		t.Fatalf("never work=%#v %v", refused, err)
	}
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := svc.ReleaseDelivery(ctx, service.ReleaseDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "release", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken}); !hasCode(err, service.CodeLeaseExpired) {
		t.Fatalf("equality release=%v", err)
	}
	settled, err := svc.SettlePending(ctx)
	if err != nil || len(settled) != 1 || settled[0].State != "queued" {
		t.Fatalf("release stale claim=%#v %v", settled, err)
	}
}

func TestReplacementInstanceReconcilesDispatchButCannotUseOldClaim(t *testing.T) {
	ctx := context.Background()
	_, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first := submit(t, svc, alpha, "send-1", "first", time.Hour)
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-1", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin-1", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "native-1"}); err != nil {
		t.Fatal(err)
	}
	replacement := registerWithPrevious(t, svc, beta.AdapterID, "recipient-2", beta.LeaseToken)
	if _, err := svc.ReleaseDelivery(ctx, service.ReleaseDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "old-release", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken}); !hasCode(err, service.CodeLeaseFenced) {
		t.Fatalf("old instance=%v", err)
	}
	ack, err := svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: replacement.AdapterID, LeaseToken: replacement.LeaseToken, OperationID: "replacement-ack", DeliveryID: first.Delivery.DeliveryID, NativeAttemptRef: "native-1"})
	if err != nil || ack.State != "delivered" {
		t.Fatalf("replacement reconciliation=%#v %v", ack, err)
	}
}

func TestClaimAtTTLExpiryTerminalizesExpired(t *testing.T) {
	ctx := context.Background()
	clock, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first := submit(t, svc, alpha, "send-expiring", "first", time.Minute)
	clock.Advance(time.Minute)
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-expired", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil || claim.Claimed || claim.Reason != "no_work" {
		t.Fatalf("claim expired=%#v %v", claim, err)
	}
	delivery, err := svc.GetDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || delivery.State != "expired" || delivery.TerminalReason != "ttl_expired" {
		t.Fatalf("expired delivery=%#v %v", delivery, err)
	}
}

func TestCancelClaimedReturnsStoredTerminalRecord(t *testing.T) {
	ctx := context.Background()
	_, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first := submit(t, svc, alpha, "send-cancel", "first", time.Hour)
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-cancel", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := svc.CancelDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != "cancelled" || cancelled.ClaimOwnerAdapterID != "" || cancelled.ClaimOwnerInstanceID != "" || cancelled.ClaimExpiresAt != nil || cancelled.ClaimedAt != nil || cancelled.DispatchAction != "" {
		t.Fatalf("cancel response retained claim metadata: %#v", cancelled)
	}
	persisted, err := svc.GetDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || !reflect.DeepEqual(persisted, cancelled) {
		t.Fatalf("cancel readback=%#v err=%v response=%#v", persisted, err, cancelled)
	}
	_ = claim
}

func TestDispatchReconcilesAfterRebindByDurableAttemptOwner(t *testing.T) {
	ctx := context.Background()
	_, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	first := submit(t, svc, alpha, "send-rebind", "first", time.Hour)
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-rebind", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin-rebind", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "native-rebind"}); err != nil {
		t.Fatal(err)
	}
	gamma := register(t, svc, "adapter.gamma", "gamma-1")
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: gamma.AdapterID, TargetRef: "recipient-next", ExpectedRevision: pointer(bob.Revision)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: gamma.AdapterID, LeaseToken: gamma.LeaseToken, OperationID: "gamma-ack", DeliveryID: first.Delivery.DeliveryID, NativeAttemptRef: "native-rebind"}); !hasCode(err, service.CodeAdapterMismatch) {
		t.Fatalf("different adapter reconcile=%v", err)
	}
	replacement := registerWithPrevious(t, svc, beta.AdapterID, "recipient-2", beta.LeaseToken)
	ack, err := svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: replacement.AdapterID, LeaseToken: replacement.LeaseToken, OperationID: "beta-ack", DeliveryID: first.Delivery.DeliveryID, NativeAttemptRef: "native-rebind"})
	if err != nil || ack.State != "delivered" {
		t.Fatalf("owner reconcile=%#v %v", ack, err)
	}
}

func TestPendingNotificationIsDurableNoClaim(t *testing.T) {
	ctx := context.Background()
	_, svc, closeDB, alpha, beta, bob := deliveryFixture(t)
	defer closeDB()
	if _, err := svc.PutBinding(ctx, service.PutBindingRequest{Address: "agent/bob", ActorAdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, AdapterID: beta.AdapterID, TargetRef: bob.TargetRef, Capabilities: []string{"pending_notification"}, ExpectedRevision: pointer(bob.Revision)}); err != nil {
		t.Fatal(err)
	}
	first := submit(t, svc, alpha, "send-notice", "first", time.Hour)
	result, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-notice", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "busy", ClaimDuration: time.Minute})
	if err != nil || result.Claimed || result.Reason != "pending_notification" || result.DispatchAction != "" {
		t.Fatalf("notification claim=%#v %v", result, err)
	}
	delivery, err := svc.GetDelivery(ctx, first.Delivery.DeliveryID)
	if err != nil || delivery.State != "queued" {
		t.Fatalf("notification state=%#v %v", delivery, err)
	}
	if _, err := svc.Acknowledge(ctx, service.ReconcileDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "notice-ack", DeliveryID: first.Delivery.DeliveryID, NativeAttemptRef: "none"}); !hasCode(err, service.CodeConflict) {
		t.Fatalf("notification acknowledge=%v", err)
	}
}

func TestDispatchReaperUsesOwnerLeaseNotOldClaimTTL(t *testing.T) {
	ctx := context.Background()
	clock := &mutableClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	persistence, svc := newService(t, filepath.Join(t.TempDir(), "owner-lease.db"), clock, tokenSequence("alpha", "beta", "claim"))
	defer persistence.Close()
	alpha, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.alpha", InstanceID: "alpha", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: "adapter.beta", InstanceID: "beta", LeaseDuration: time.Minute})
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
	first, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "send", SenderAddress: "agent/alice", RecipientAddress: "agent/bob", Body: "first", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim", RecipientAddress: "agent/bob", RecipientGeneration: bob.Generation, Availability: "idle", ClaimDuration: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginDispatch(ctx, service.BeginDispatchRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "begin", DeliveryID: first.Delivery.DeliveryID, ClaimToken: claim.ClaimToken, NativeAttemptRef: "native"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	settled, err := svc.SettlePending(ctx)
	if err != nil || len(settled) != 1 || settled[0].State != "outcome_unknown" {
		t.Fatalf("owner lease reaper=%#v %v", settled, err)
	}
}

func contains(value, part string) bool {
	return len(value) >= len(part) && (func() bool {
		for i := 0; i+len(part) <= len(value); i++ {
			if value[i:i+len(part)] == part {
				return true
			}
		}
		return false
	})()
}
