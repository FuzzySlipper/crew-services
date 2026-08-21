// Package store defines the persistence boundary used by service logic.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound                = errors.New("not found")
	ErrConflict                = errors.New("conflict")
	ErrStaleRevision           = errors.New("stale revision")
	ErrLeaseExpired            = errors.New("lease expired")
	ErrLeaseFenced             = errors.New("lease fenced")
	ErrNotBound                = errors.New("not currently bound")
	ErrAdapterMismatch         = errors.New("adapter does not match binding")
	ErrClaimFenced             = errors.New("claim is fenced")
	ErrOperationConflict       = errors.New("operation ID was reused with different message content")
	ErrReplyOriginalNotFound   = errors.New("reply original message was not found")
	ErrReplySenderMismatch     = errors.New("reply sender does not match original recipient")
	ErrReplyRecipientMismatch  = errors.New("reply recipient does not match original sender")
	ErrReplyGenerationMismatch = errors.New("reply binding generations do not match original")
)

// AdapterLease is the fenced, replaceable instance lease for a stable adapter.
// The token is intentionally omitted from binding read models.
type AdapterLease struct {
	AdapterID  string    `json:"adapter_id"`
	InstanceID string    `json:"instance_id"`
	LeaseToken string    `json:"lease_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Binding is the durable state for one fabric-owned logical address. An
// unbound address has Bound false and no routing fields, but retains revision
// and generation history.
type Binding struct {
	Address      string   `json:"address"`
	Bound        bool     `json:"bound"`
	AdapterID    string   `json:"adapter_id,omitempty"`
	TargetRef    string   `json:"target_ref,omitempty"`
	Capabilities []string `json:"capabilities"`
	Revision     int64    `json:"revision"`
	Generation   int64    `json:"generation"`
}

type RegisterAdapterRequest struct {
	AdapterID          string
	InstanceID         string
	LeaseToken         string
	PreviousLeaseToken string
	ExpiresAt          time.Time
}

type RenewAdapterRequest struct {
	AdapterID  string
	LeaseToken string
	ExpiresAt  time.Time
}

type PutBindingRequest struct {
	Address          string
	ActorAdapterID   string
	LeaseToken       string
	AdapterID        string
	TargetRef        string
	Capabilities     []string
	ExpectedRevision *int64
}

type UnbindRequest struct {
	Address          string
	ActorAdapterID   string
	LeaseToken       string
	ExpectedRevision int64
}

type SenderFenceRequest struct {
	AdapterID     string
	LeaseToken    string
	SenderAddress string
}

// Message is the immutable, accepted envelope. It records only fabric facts;
// native runtime insertion and processing belong to adapters and later work.
type Message struct {
	ProducerID          string    `json:"producer_id"`
	OperationID         string    `json:"operation_id"`
	MessageID           string    `json:"message_id"`
	SenderAddress       string    `json:"sender_address"`
	RecipientAddress    string    `json:"recipient_address"`
	Body                string    `json:"body"`
	CorrelationID       string    `json:"correlation_id,omitempty"`
	ReplyToMessageID    string    `json:"reply_to_message_id,omitempty"`
	ActivationPolicy    string    `json:"activation_policy"`
	CreatedAt           time.Time `json:"created_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	SenderGeneration    int64     `json:"sender_generation"`
	RecipientGeneration int64     `json:"recipient_generation"`
}

// Delivery is the mutable ledger entry for one immutable message. This slice
// Claim tokens are deliberately omitted from this observational record.
type Delivery struct {
	DeliveryID           string `json:"delivery_id"`
	MessageID            string `json:"message_id"`
	RecipientAddress     string `json:"recipient_address"`
	RecipientGeneration  int64  `json:"recipient_generation"`
	AcceptedSequence     int64  `json:"accepted_sequence"`
	State                string `json:"state"`
	AttemptCount         int    `json:"attempt_count"`
	ClaimOwnerAdapterID  string `json:"claim_owner_adapter_id,omitempty"`
	ClaimOwnerInstanceID string `json:"claim_owner_instance_id,omitempty"`
	// ClaimToken is persistence-only and deliberately excluded from every
	// observational JSON representation; only ClaimResult returns a token.
	ClaimToken       string     `json:"-"`
	ClaimExpiresAt   *time.Time `json:"claim_expires_at,omitempty"`
	ClaimedAt        *time.Time `json:"claimed_at,omitempty"`
	DispatchAction   string     `json:"dispatch_action,omitempty"`
	NativeAttemptRef string     `json:"native_attempt_ref,omitempty"`
	DispatchingAt    *time.Time `json:"dispatching_at,omitempty"`
	TerminalReason   string     `json:"terminal_reason,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	TerminalAt       *time.Time `json:"terminal_at,omitempty"`
}

type SubmitMessageRequest struct {
	ProducerID       string
	LeaseToken       string
	OperationID      string
	Fingerprint      string
	MessageID        string
	DeliveryID       string
	SenderAddress    string
	RecipientAddress string
	Body             string
	CorrelationID    string
	ReplyToMessageID string
	ActivationPolicy string
	ExpiresAt        time.Time
}

type SubmitMessageResult struct {
	Message  Message  `json:"message"`
	Delivery Delivery `json:"delivery"`
	Replayed bool     `json:"replayed"`
}

// Round is the optional single-resolution layer above an immutable root
// message. It never changes the message or delivery it records.
type Round struct {
	RoundID             string     `json:"round_id"`
	ProducerID          string     `json:"producer_id"`
	OperationID         string     `json:"operation_id"`
	RootMessageID       string     `json:"root_message_id"`
	SenderAddress       string     `json:"sender_address"`
	RecipientAddress    string     `json:"recipient_address"`
	SenderGeneration    int64      `json:"sender_generation"`
	RecipientGeneration int64      `json:"recipient_generation"`
	CorrelationID       string     `json:"correlation_id,omitempty"`
	Status              string     `json:"status"`
	ReplyMessageID      string     `json:"reply_message_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	TerminalAt          *time.Time `json:"terminal_at,omitempty"`
	TerminalReason      string     `json:"terminal_reason,omitempty"`
	Revision            int64      `json:"revision"`
}

type BeginRoundRequest struct {
	SubmitMessageRequest
	RoundID        string
	RoundExpiresAt time.Time
}
type BeginRoundResult struct {
	Round    Round    `json:"round"`
	Message  Message  `json:"message"`
	Delivery Delivery `json:"delivery"`
	Replayed bool     `json:"replayed"`
}
type RoundOperationLookup struct {
	Found       bool
	Fingerprint string
	Result      BeginRoundResult
}

// Traffic is a read-only, status-faithful snapshot. Its arrays deliberately
// remain separate: a round is not inferred from a delivery or correlation.
type Traffic struct {
	Messages   []Message  `json:"messages"`
	Deliveries []Delivery `json:"deliveries"`
	Rounds     []Round    `json:"rounds"`
}

// OperationLookup is the side-effect-free producer-scoped idempotency read.
// SubmitMessage still repeats this check inside its write transaction to close
// concurrent first-submit races.
type OperationLookup struct {
	Found       bool
	Fingerprint string
	Message     Message
	Delivery    Delivery
}

// Store is the minimal persistence surface required by the initial service.
type Store interface {
	Ready(context.Context) error
	Close() error
	RegisterAdapter(context.Context, time.Time, RegisterAdapterRequest) (AdapterLease, error)
	RenewAdapter(context.Context, time.Time, RenewAdapterRequest) (AdapterLease, error)
	PutBinding(context.Context, time.Time, PutBindingRequest) (Binding, error)
	Unbind(context.Context, time.Time, UnbindRequest) (Binding, error)
	Resolve(context.Context, string) (Binding, error)
	List(context.Context) ([]Binding, error)
	FenceSender(context.Context, time.Time, SenderFenceRequest) (Binding, error)
	LookupMessageOperation(context.Context, string, string) (OperationLookup, error)
	SubmitMessage(context.Context, time.Time, SubmitMessageRequest) (SubmitMessageResult, error)
	LookupRoundOperation(context.Context, string, string) (RoundOperationLookup, error)
	BeginRound(context.Context, time.Time, BeginRoundRequest) (BeginRoundResult, error)
	GetRound(context.Context, string) (Round, error)
	ListRounds(context.Context) ([]Round, error)
	CancelRound(context.Context, time.Time, string) (Round, error)
	SettleRounds(context.Context, time.Time) ([]Round, error)
	Traffic(context.Context) (Traffic, error)
	GetMessage(context.Context, string) (Message, error)
	ListMessages(context.Context) ([]Message, error)
	GetDelivery(context.Context, string) (Delivery, error)
	ListDeliveries(context.Context) ([]Delivery, error)
	Mailbox(context.Context, string) ([]Delivery, error)
	HeadDelivery(context.Context, string, int64) (*Delivery, error)
	CancelDelivery(context.Context, time.Time, string) (Delivery, error)
	SettlePending(context.Context, time.Time) ([]Delivery, error)
	LookupDeliveryOperation(context.Context, string, string) (DeliveryOperationLookup, error)
	DeliveryOperation(context.Context, time.Time, DeliveryOperationRequest) (DeliveryOperationResult, error)
}
