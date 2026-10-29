package sqlite

import (
	"testing"

	"crew-services/internal/store"
)

func TestClaimActionPolicyTable(t *testing.T) {
	steer := []string{store.CapabilityDeliverWhenIdle, store.CapabilityDurableNextTurn, store.CapabilityWakeInactive, store.CapabilityDeliverNextStep}
	plain := []string{store.CapabilityDeliverWhenIdle, store.CapabilityDurableNextTurn, store.CapabilityWakeInactive}
	idleOnly := []string{store.CapabilityDeliverWhenIdle}
	cases := []struct {
		policy, availability string
		caps                 []string
		action, reason       string
	}{
		// next_turn never reaches an active turn.
		{store.PolicyNextTurn, store.AvailabilityBusy, steer, "register_next_turn", ""},
		{store.PolicyNextTurn, store.AvailabilityIdle, steer, "deliver_at_idle", ""},
		{store.PolicyNextTurn, store.AvailabilityInactive, steer, "wake_then_deliver", ""},
		{store.PolicyNextTurn, store.AvailabilityBusy, idleOnly, "", "not_eligible"},
		// next_step reaches the active turn when the adapter can, and otherwise
		// waits for the next turn: later than asked, never earlier.
		{store.PolicyNextStep, store.AvailabilityBusy, steer, "deliver_next_step", ""},
		{store.PolicyNextStep, store.AvailabilityBusy, plain, "register_next_turn", ""},
		{store.PolicyNextStep, store.AvailabilityBusy, idleOnly, "", "not_eligible"},
		{store.PolicyNextStep, store.AvailabilityIdle, steer, "deliver_at_idle", ""},
		{store.PolicyNextStep, store.AvailabilityInactive, steer, "wake_then_deliver", ""},
		{store.PolicyNextStep, store.AvailabilityInactive, idleOnly, "", "not_eligible"},
		// no_wake never starts a turn.
		{store.PolicyNoWake, store.AvailabilityBusy, steer, "deliver_next_step", ""},
		{store.PolicyNoWake, store.AvailabilityBusy, plain, "", "not_eligible"},
		{store.PolicyNoWake, store.AvailabilityIdle, steer, "", "not_eligible"},
		{store.PolicyNoWake, store.AvailabilityInactive, steer, "", "not_eligible"},
		// Retired names stored on older rows keep their meaning.
		{"wake_when_idle", store.AvailabilityIdle, plain, "deliver_at_idle", ""},
		{"never_wake", store.AvailabilityIdle, plain, "", "not_eligible"},
	}
	for _, c := range cases {
		action, reason := claimAction(c.policy, c.caps, c.availability)
		if action != c.action || reason != c.reason {
			t.Errorf("claimAction(%s, %v, %s) = %q, %q; want %q, %q", c.policy, c.caps, c.availability, action, reason, c.action, c.reason)
		}
	}
}

func TestNormalizePolicy(t *testing.T) {
	for in, want := range map[string]string{"": "next_turn", "next_turn": "next_turn", "wake_when_idle": "next_turn", "next_step": "next_step", "no_wake": "no_wake", "never_wake": "no_wake"} {
		if got, ok := store.NormalizePolicy(in); !ok || got != want {
			t.Errorf("NormalizePolicy(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := store.NormalizePolicy("interrupt"); ok {
		t.Error("NormalizePolicy accepted interrupt")
	}
}
