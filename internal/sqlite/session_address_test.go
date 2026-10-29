package sqlite_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"crew-services/internal/service"
)

func TestSessionAddressRegistrationIsStableAndKeepsQueuedWork(t *testing.T) {
	ctx := context.Background()
	_, svc, closeDB, alpha, beta, _ := deliveryFixture(t)
	defer closeDB()
	caps := []string{"deliver_when_idle", "deliver_next_step"}
	first, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, NativeKey: "native-session-1", Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Address, service.SessionAddressPrefix) || strings.Contains(first.Address, "native-session-1") || first.TargetRef != "native-session-1" || first.Generation != 1 {
		t.Fatalf("first registration=%#v", first)
	}
	if first.Address != service.SessionAddress(beta.AdapterID, "native-session-1") {
		t.Fatalf("address %q is not derived from the adapter and native key", first.Address)
	}

	again, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, NativeKey: "native-session-1", Capabilities: []string{"deliver_next_step", "deliver_when_idle"}})
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("repeat registration=%#v %v, want unchanged %#v", again, err, first)
	}

	queued, err := svc.SubmitMessage(ctx, service.SubmitMessageRequest{ProducerID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, OperationID: "to-session", SenderAddress: "agent/alice", RecipientAddress: first.Address, Body: "gate failed", ActivationPolicy: "next_step", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	widened, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, NativeKey: "native-session-1", Capabilities: append(caps, "wake_inactive")})
	if err != nil || widened.Generation != first.Generation || widened.Revision != first.Revision+1 {
		t.Fatalf("capability change=%#v %v", widened, err)
	}
	claim, err := svc.ClaimDelivery(ctx, service.ClaimDeliveryRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken, OperationID: "claim-session", RecipientAddress: first.Address, RecipientGeneration: widened.Generation, Availability: "busy", ClaimDuration: time.Minute})
	if err != nil || !claim.Claimed || claim.Delivery.DeliveryID != queued.Delivery.DeliveryID || claim.DispatchAction != "deliver_next_step" {
		t.Fatalf("claim after re-registration=%#v %v", claim, err)
	}

	if _, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: alpha.AdapterID, LeaseToken: alpha.LeaseToken, NativeKey: "native-session-1"}); err != nil {
		t.Fatalf("another adapter's own native key must get its own address: %v", err)
	}
	other := service.SessionAddress(alpha.AdapterID, "native-session-1")
	if other == first.Address {
		t.Fatal("two adapters' sessions share an address")
	}
	if _, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: beta.AdapterID, LeaseToken: beta.LeaseToken}); !hasCode(err, service.CodeInvalid) {
		t.Fatalf("missing native key=%v", err)
	}
	if _, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: beta.AdapterID, LeaseToken: "stale", NativeKey: "native-session-2"}); err == nil {
		t.Fatal("registration without a live lease succeeded")
	}
}
