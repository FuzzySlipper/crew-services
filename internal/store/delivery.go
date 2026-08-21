package store

import "time"

// Delivery capability constants are deliberately runtime-neutral.
const (
	CapabilityDeliverWhenIdle = "deliver_when_idle"
	CapabilityDurableNextTurn = "durable_next_turn"
	CapabilityWakeInactive    = "wake_inactive"
	CapabilityPendingNotice   = "pending_notification"
)

const (
	AvailabilityBusy     = "busy"
	AvailabilityIdle     = "idle"
	AvailabilityInactive = "inactive"
)

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
