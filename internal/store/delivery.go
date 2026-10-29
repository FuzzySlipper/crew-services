package store

import "time"

// Delivery capability constants are deliberately runtime-neutral.
const (
	CapabilityDeliverWhenIdle = "deliver_when_idle"
	CapabilityDurableNextTurn = "durable_next_turn"
	CapabilityWakeInactive    = "wake_inactive"
	CapabilityPendingNotice   = "pending_notification"
	// CapabilityDeliverNextStep means the adapter can put a message in front of
	// a busy target at its next step boundary without cancelling its turn.
	CapabilityDeliverNextStep = "deliver_next_step"
)

const (
	AvailabilityBusy     = "busy"
	AvailabilityIdle     = "idle"
	AvailabilityInactive = "inactive"
)

// Activation policies are chosen by the sender. next_turn never touches an
// active turn; next_step may be seen at the active turn's next step boundary;
// no_wake never starts a turn on its own. None of them interrupts.
const (
	PolicyNextTurn = "next_turn"
	PolicyNextStep = "next_step"
	PolicyNoWake   = "no_wake"
)

// NormalizePolicy maps an activation policy, including the retired v1 names
// stored on older messages, to its current name. It reports false for an
// unknown policy.
func NormalizePolicy(policy string) (string, bool) {
	switch policy {
	case "", PolicyNextTurn, "wake_when_idle":
		return PolicyNextTurn, true
	case PolicyNextStep:
		return PolicyNextStep, true
	case PolicyNoWake, "never_wake":
		return PolicyNoWake, true
	}
	return "", false
}

// ClaimResult is returned for both a durable no-work receipt and a claim.  A
// claim token appears only here, never in ordinary delivery read models.
type ClaimResult struct {
	Claimed        bool      `json:"claimed"`
	Reason         string    `json:"reason,omitempty"`
	Message        *Message  `json:"message,omitempty"`
	Delivery       *Delivery `json:"delivery,omitempty"`
	Head           *Delivery `json:"head,omitempty"`
	ClaimToken     string    `json:"claim_token,omitempty"`
	DispatchAction string    `json:"dispatch_action,omitempty"`
	Replayed       bool      `json:"replayed"`
}

type DeliveryOperationRequest struct {
	Kind                string
	AdapterID           string
	LeaseToken          string
	OperationID         string
	Fingerprint         string
	Address             string
	Generation          int64
	Availability        string
	ClaimDuration       time.Duration
	GeneratedClaimToken string
	DeliveryID          string
	ClaimToken          string
	NativeAttemptRef    string
	FailureReason       string
}

type DeliveryOperationLookup struct {
	Found       bool
	Fingerprint string
	Result      DeliveryOperationResult
}

type DeliveryOperationResult struct {
	Claim    ClaimResult `json:"claim,omitempty"`
	Delivery Delivery    `json:"delivery,omitempty"`
	Replayed bool        `json:"replayed"`
}
