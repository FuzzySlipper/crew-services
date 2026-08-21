// Package service contains typed application behavior independent of HTTP and SQLite.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"crew-services/internal/store"
)

// ErrorCode is a stable, transport-neutral failure classification.
type ErrorCode string

const (
	CodeInvalid           ErrorCode = "invalid_request"
	CodeNotFound          ErrorCode = "not_found"
	CodeConflict          ErrorCode = "conflict"
	CodeStaleRevision     ErrorCode = "stale_revision"
	CodeLeaseExpired      ErrorCode = "lease_expired"
	CodeLeaseFenced       ErrorCode = "lease_fenced"
	CodeNotBound          ErrorCode = "not_bound"
	CodeAdapterMismatch   ErrorCode = "adapter_mismatch"
	CodeOperationConflict ErrorCode = "operation_conflict"
	CodeInvalidState      ErrorCode = "invalid_delivery_state"
	CodeClaimFenced       ErrorCode = "claim_fenced"
)

// Error carries a stable code while preserving a useful local cause.
type Error struct {
	Code ErrorCode
	Err  error
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// AdapterLease and Binding are service-facing aliases of the durable records.
type AdapterLease = store.AdapterLease
type Binding = store.Binding

type RegisterAdapterRequest struct {
	AdapterID          string
	InstanceID         string
	PreviousLeaseToken string
	LeaseDuration      time.Duration
}

type RenewAdapterRequest struct {
	AdapterID     string
	LeaseToken    string
	LeaseDuration time.Duration
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

type Message = store.Message
type Delivery = store.Delivery

type SubmitMessageRequest struct {
	ProducerID       string
	LeaseToken       string
	OperationID      string
	SenderAddress    string
	RecipientAddress string
	Body             string
	CorrelationID    string
	ReplyToMessageID string
	ActivationPolicy string
	TTL              time.Duration
}

type SubmitMessageResult = store.SubmitMessageResult

// Clock supplies the current time to service behavior.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock implementation.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Status is the typed readiness result exposed by a transport.
type Status struct {
	Status string    `json:"status"`
	Time   time.Time `json:"time"`
}

// Service coordinates application behavior through injected dependencies.
type Service struct {
	store            store.Store
	clock            Clock
	maxLeaseDuration time.Duration
	maxTTLDuration   time.Duration
	tokens           TokenGenerator
	ids              IDGenerator
}

// TokenGenerator creates opaque coordination tokens. It is injectable for
// deterministic tests; tokens are not a public authentication mechanism.
type TokenGenerator func() (string, error)

// IDGenerator creates service-owned immutable message and delivery IDs.
type IDGenerator func() (string, error)

type Option func(*Service) error

func WithMaxLeaseDuration(value time.Duration) Option {
	return func(s *Service) error {
		if value <= 0 {
			return errors.New("maximum lease duration must be positive")
		}
		s.maxLeaseDuration = value
		return nil
	}
}

func WithTokenGenerator(generator TokenGenerator) Option {
	return func(s *Service) error {
		if generator == nil {
			return errors.New("token generator is required")
		}
		s.tokens = generator
		return nil
	}
}

func WithMaxTTLDuration(value time.Duration) Option {
	return func(s *Service) error {
		if value <= 0 {
			return errors.New("maximum TTL duration must be positive")
		}
		s.maxTTLDuration = value
		return nil
	}
}

func WithIDGenerator(generator IDGenerator) Option {
	return func(s *Service) error {
		if generator == nil {
			return errors.New("ID generator is required")
		}
		s.ids = generator
		return nil
	}
}

// New constructs a service with explicit persistence and time dependencies.
func New(persistence store.Store, clock Clock, options ...Option) (*Service, error) {
	if persistence == nil {
		return nil, errors.New("store is required")
	}
	if clock == nil {
		return nil, errors.New("clock is required")
	}
	svc := &Service{store: persistence, clock: clock, maxLeaseDuration: 5 * time.Minute, maxTTLDuration: 24 * time.Hour, tokens: randomToken, ids: randomToken}
	for _, option := range options {
		if err := option(svc); err != nil {
			return nil, err
		}
	}
	return svc, nil
}

// SubmitMessage atomically records an immutable envelope and exactly one
// queued ledger entry. The store checks existing idempotency before current
// authority, so exact retries survive lease and binding changes.
func (s *Service) SubmitMessage(ctx context.Context, request SubmitMessageRequest) (SubmitMessageResult, error) {
	for _, field := range []struct{ name, value string }{{"producer_id", request.ProducerID}, {"lease_token", request.LeaseToken}, {"operation_id", request.OperationID}, {"sender_address", request.SenderAddress}, {"recipient_address", request.RecipientAddress}} {
		if err := required(field.name, field.value); err != nil {
			return SubmitMessageResult{}, err
		}
	}
	if strings.TrimSpace(request.Body) == "" {
		return SubmitMessageResult{}, &Error{Code: CodeInvalid, Err: errors.New("body is required")}
	}
	request.ProducerID = strings.TrimSpace(request.ProducerID)
	request.OperationID = strings.TrimSpace(request.OperationID)
	request.SenderAddress = strings.TrimSpace(request.SenderAddress)
	request.RecipientAddress = strings.TrimSpace(request.RecipientAddress)
	request.CorrelationID = strings.TrimSpace(request.CorrelationID)
	request.ReplyToMessageID = strings.TrimSpace(request.ReplyToMessageID)
	if request.ActivationPolicy == "" {
		request.ActivationPolicy = "wake_when_idle"
	}
	if request.ActivationPolicy != "wake_when_idle" && request.ActivationPolicy != "never_wake" {
		return SubmitMessageResult{}, &Error{Code: CodeInvalid, Err: errors.New("activation_policy must be wake_when_idle or never_wake")}
	}
	if request.TTL <= 0 || request.TTL > s.maxTTLDuration {
		return SubmitMessageResult{}, &Error{Code: CodeInvalid, Err: fmt.Errorf("ttl must be greater than zero and at most %s", s.maxTTLDuration)}
	}
	fingerprint, err := messageFingerprint(request)
	if err != nil {
		return SubmitMessageResult{}, fmt.Errorf("fingerprint message request: %w", err)
	}
	lookup, err := s.store.LookupMessageOperation(ctx, request.ProducerID, request.OperationID)
	if err != nil {
		return SubmitMessageResult{}, mapStoreError(err)
	}
	if lookup.Found {
		if lookup.Fingerprint != fingerprint {
			return SubmitMessageResult{}, &Error{Code: CodeOperationConflict, Err: store.ErrOperationConflict}
		}
		return SubmitMessageResult{Message: lookup.Message, Delivery: lookup.Delivery, Replayed: true}, nil
	}
	messageID, err := s.ids()
	if err != nil {
		return SubmitMessageResult{}, fmt.Errorf("generate message ID: %w", err)
	}
	deliveryID, err := s.ids()
	if err != nil {
		return SubmitMessageResult{}, fmt.Errorf("generate delivery ID: %w", err)
	}
	now := s.clock.Now().UTC()
	result, err := s.store.SubmitMessage(ctx, now, store.SubmitMessageRequest{ProducerID: request.ProducerID, LeaseToken: request.LeaseToken, OperationID: request.OperationID, Fingerprint: fingerprint, MessageID: messageID, DeliveryID: deliveryID, SenderAddress: request.SenderAddress, RecipientAddress: request.RecipientAddress, Body: request.Body, CorrelationID: request.CorrelationID, ReplyToMessageID: request.ReplyToMessageID, ActivationPolicy: request.ActivationPolicy, ExpiresAt: now.Add(request.TTL)})
	return result, mapReplyError(err)
}

func messageFingerprint(request SubmitMessageRequest) (string, error) {
	canonical := struct {
		SenderAddress, RecipientAddress, Body, CorrelationID, ReplyToMessageID, ActivationPolicy string
		TTLNanoseconds                                                                           int64
	}{request.SenderAddress, request.RecipientAddress, request.Body, request.CorrelationID, request.ReplyToMessageID, request.ActivationPolicy, request.TTL.Nanoseconds()}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) GetMessage(ctx context.Context, messageID string) (Message, error) {
	if err := required("message_id", messageID); err != nil {
		return Message{}, err
	}
	message, err := s.store.GetMessage(ctx, strings.TrimSpace(messageID))
	return message, mapStoreError(err)
}
func (s *Service) ListMessages(ctx context.Context) ([]Message, error) {
	values, err := s.store.ListMessages(ctx)
	return values, mapStoreError(err)
}
func (s *Service) GetDelivery(ctx context.Context, deliveryID string) (Delivery, error) {
	if err := required("delivery_id", deliveryID); err != nil {
		return Delivery{}, err
	}
	value, err := s.store.GetDelivery(ctx, strings.TrimSpace(deliveryID))
	return value, mapStoreError(err)
}
func (s *Service) ListDeliveries(ctx context.Context) ([]Delivery, error) {
	values, err := s.store.ListDeliveries(ctx)
	return values, mapStoreError(err)
}
func (s *Service) Mailbox(ctx context.Context, address string) ([]Delivery, error) {
	if err := required("recipient_address", address); err != nil {
		return nil, err
	}
	values, err := s.store.Mailbox(ctx, strings.TrimSpace(address))
	return values, mapStoreError(err)
}
func (s *Service) HeadDelivery(ctx context.Context, address string, generation int64) (*Delivery, error) {
	if err := required("recipient_address", address); err != nil {
		return nil, err
	}
	if generation <= 0 {
		return nil, &Error{Code: CodeInvalid, Err: errors.New("recipient_generation must be positive")}
	}
	value, err := s.store.HeadDelivery(ctx, strings.TrimSpace(address), generation)
	return value, mapStoreError(err)
}
func (s *Service) CancelDelivery(ctx context.Context, deliveryID string) (Delivery, error) {
	if err := required("delivery_id", deliveryID); err != nil {
		return Delivery{}, err
	}
	value, err := s.store.CancelDelivery(ctx, s.clock.Now().UTC(), strings.TrimSpace(deliveryID))
	return value, mapStoreError(err)
}
func (s *Service) SettlePending(ctx context.Context) ([]Delivery, error) {
	values, err := s.store.SettlePending(ctx, s.clock.Now().UTC())
	return values, mapStoreError(err)
}

func randomToken() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (s *Service) RegisterAdapter(ctx context.Context, request RegisterAdapterRequest) (AdapterLease, error) {
	if err := required("adapter_id", request.AdapterID); err != nil {
		return AdapterLease{}, err
	}
	if err := required("instance_id", request.InstanceID); err != nil {
		return AdapterLease{}, err
	}
	duration, err := s.leaseDuration(request.LeaseDuration)
	if err != nil {
		return AdapterLease{}, err
	}
	token, err := s.tokens()
	if err != nil {
		return AdapterLease{}, fmt.Errorf("create adapter lease: %w", err)
	}
	now := s.clock.Now().UTC()
	lease, err := s.store.RegisterAdapter(ctx, now, store.RegisterAdapterRequest{AdapterID: request.AdapterID, InstanceID: request.InstanceID, LeaseToken: token, PreviousLeaseToken: request.PreviousLeaseToken, ExpiresAt: now.Add(duration)})
	return lease, mapStoreError(err)
}

func (s *Service) RenewAdapter(ctx context.Context, request RenewAdapterRequest) (AdapterLease, error) {
	if err := required("adapter_id", request.AdapterID); err != nil {
		return AdapterLease{}, err
	}
	if err := required("lease_token", request.LeaseToken); err != nil {
		return AdapterLease{}, err
	}
	duration, err := s.leaseDuration(request.LeaseDuration)
	if err != nil {
		return AdapterLease{}, err
	}
	now := s.clock.Now().UTC()
	lease, err := s.store.RenewAdapter(ctx, now, store.RenewAdapterRequest{AdapterID: request.AdapterID, LeaseToken: request.LeaseToken, ExpiresAt: now.Add(duration)})
	return lease, mapStoreError(err)
}

func (s *Service) PutBinding(ctx context.Context, request PutBindingRequest) (Binding, error) {
	for _, field := range []struct{ name, value string }{{"address", request.Address}, {"actor_adapter_id", request.ActorAdapterID}, {"lease_token", request.LeaseToken}, {"adapter_id", request.AdapterID}, {"target_ref", request.TargetRef}} {
		if err := required(field.name, field.value); err != nil {
			return Binding{}, err
		}
	}
	capabilities, err := canonicalCapabilities(request.Capabilities)
	if err != nil {
		return Binding{}, err
	}
	binding, err := s.store.PutBinding(ctx, s.clock.Now().UTC(), store.PutBindingRequest{Address: request.Address, ActorAdapterID: request.ActorAdapterID, LeaseToken: request.LeaseToken, AdapterID: request.AdapterID, TargetRef: request.TargetRef, Capabilities: capabilities, ExpectedRevision: request.ExpectedRevision})
	return binding, mapStoreError(err)
}

func (s *Service) Unbind(ctx context.Context, request UnbindRequest) (Binding, error) {
	for _, field := range []struct{ name, value string }{{"address", request.Address}, {"actor_adapter_id", request.ActorAdapterID}, {"lease_token", request.LeaseToken}} {
		if err := required(field.name, field.value); err != nil {
			return Binding{}, err
		}
	}
	binding, err := s.store.Unbind(ctx, s.clock.Now().UTC(), store.UnbindRequest(request))
	return binding, mapStoreError(err)
}

func (s *Service) Resolve(ctx context.Context, address string) (Binding, error) {
	if err := required("address", address); err != nil {
		return Binding{}, err
	}
	binding, err := s.store.Resolve(ctx, address)
	return binding, mapStoreError(err)
}

func (s *Service) List(ctx context.Context) ([]Binding, error) {
	bindings, err := s.store.List(ctx)
	return bindings, mapStoreError(err)
}

// FenceSender is the typed guard future message submission will use before it
// accepts an address as an adapter's sender.
func (s *Service) FenceSender(ctx context.Context, request SenderFenceRequest) (Binding, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", request.AdapterID}, {"lease_token", request.LeaseToken}, {"sender_address", request.SenderAddress}} {
		if err := required(field.name, field.value); err != nil {
			return Binding{}, err
		}
	}
	binding, err := s.store.FenceSender(ctx, s.clock.Now().UTC(), store.SenderFenceRequest(request))
	return binding, mapStoreError(err)
}

func (s *Service) leaseDuration(value time.Duration) (time.Duration, error) {
	if value <= 0 || value > s.maxLeaseDuration {
		return 0, &Error{Code: CodeInvalid, Err: fmt.Errorf("lease_duration must be greater than zero and at most %s", s.maxLeaseDuration)}
	}
	return value, nil
}

func required(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return &Error{Code: CodeInvalid, Err: fmt.Errorf("%s is required", name)}
	}
	return nil
}

func canonicalCapabilities(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != strings.TrimSpace(value) || value == "" {
			return nil, &Error{Code: CodeInvalid, Err: errors.New("capabilities must contain non-empty trimmed strings")}
		}
		seen[value] = struct{}{}
	}
	canonical := make([]string, 0, len(seen))
	for value := range seen {
		canonical = append(canonical, value)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Error); ok {
		return err
	}
	for _, candidate := range []struct {
		target error
		code   ErrorCode
	}{
		{store.ErrNotFound, CodeNotFound}, {store.ErrConflict, CodeConflict}, {store.ErrOperationConflict, CodeOperationConflict}, {store.ErrStaleRevision, CodeStaleRevision}, {store.ErrLeaseExpired, CodeLeaseExpired}, {store.ErrLeaseFenced, CodeLeaseFenced}, {store.ErrClaimFenced, CodeClaimFenced}, {store.ErrNotBound, CodeNotBound}, {store.ErrAdapterMismatch, CodeAdapterMismatch},
	} {
		if errors.Is(err, candidate.target) {
			return &Error{Code: candidate.code, Err: err}
		}
	}
	return err
}

// Ready checks persistence and returns an observable local status.
func (s *Service) Ready(ctx context.Context) (Status, error) {
	if err := s.store.Ready(ctx); err != nil {
		return Status{}, err
	}
	return Status{Status: "ok", Time: s.clock.Now().UTC()}, nil
}
