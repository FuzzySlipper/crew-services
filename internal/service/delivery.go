package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/store"
)

type ClaimDeliveryRequest struct {
	AdapterID, LeaseToken, OperationID, RecipientAddress, Availability string
	RecipientGeneration                                                int64
	ClaimDuration                                                      time.Duration
}
type ClaimDeliveryResult = store.ClaimResult

type RenewClaimRequest struct {
	AdapterID, LeaseToken, OperationID, DeliveryID, ClaimToken string
	ClaimDuration                                              time.Duration
}
type ReleaseDeliveryRequest struct{ AdapterID, LeaseToken, OperationID, DeliveryID, ClaimToken string }
type BeginDispatchRequest struct{ AdapterID, LeaseToken, OperationID, DeliveryID, ClaimToken, NativeAttemptRef string }
type ReconcileDeliveryRequest struct{ AdapterID, LeaseToken, OperationID, DeliveryID, NativeAttemptRef string }
type FailDeliveryRequest struct{ AdapterID, LeaseToken, OperationID, DeliveryID, ClaimToken, NativeAttemptRef string }

// ClaimDelivery records a durable claim or a durable no-work decision. A
// pending notification is intentionally not an action and never becomes a
// delivery fact.
func (s *Service) ClaimDelivery(ctx context.Context, r ClaimDeliveryRequest) (ClaimDeliveryResult, error) {
	if err := requireFields(r.AdapterID, r.LeaseToken, r.OperationID, r.RecipientAddress); err != nil {
		return ClaimDeliveryResult{}, err
	}
	if r.RecipientGeneration <= 0 {
		return ClaimDeliveryResult{}, invalid("recipient_generation must be positive")
	}
	if r.Availability != store.AvailabilityBusy && r.Availability != store.AvailabilityIdle && r.Availability != store.AvailabilityInactive {
		return ClaimDeliveryResult{}, invalid("availability must be busy, idle, or inactive")
	}
	d, err := s.leaseDuration(r.ClaimDuration)
	if err != nil {
		return ClaimDeliveryResult{}, err
	}
	fp := deliveryFingerprint("claim", r.RecipientAddress, r.RecipientGeneration, r.Availability, d.String())
	if result, found, err := s.replayDeliveryOperation(ctx, r.AdapterID, r.OperationID, fp); err != nil || found {
		if err != nil {
			return ClaimDeliveryResult{}, err
		}
		result.Claim.Replayed = true
		return result.Claim, nil
	}
	token, err := s.tokens()
	if err != nil {
		return ClaimDeliveryResult{}, fmt.Errorf("create claim token: %w", err)
	}
	result, err := s.store.DeliveryOperation(ctx, s.clock.Now().UTC(), store.DeliveryOperationRequest{Kind: "claim", AdapterID: r.AdapterID, LeaseToken: r.LeaseToken, OperationID: r.OperationID, Fingerprint: fp, Address: r.RecipientAddress, Generation: r.RecipientGeneration, Availability: r.Availability, ClaimDuration: d, GeneratedClaimToken: token})
	return result.Claim, mapStoreError(err)
}

func (s *Service) RenewClaim(ctx context.Context, r RenewClaimRequest) (Delivery, error) {
	return s.claimMutation(ctx, "renew", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.ClaimToken, "", r.ClaimDuration)
}
func (s *Service) ReleaseDelivery(ctx context.Context, r ReleaseDeliveryRequest) (Delivery, error) {
	return s.claimMutation(ctx, "release", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.ClaimToken, "", 0)
}
func (s *Service) BeginDispatch(ctx context.Context, r BeginDispatchRequest) (Delivery, error) {
	if strings.TrimSpace(r.NativeAttemptRef) == "" {
		return Delivery{}, invalid("native_attempt_ref is required")
	}
	return s.claimMutation(ctx, "begin_dispatch", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.ClaimToken, r.NativeAttemptRef, 0)
}
func (s *Service) Acknowledge(ctx context.Context, r ReconcileDeliveryRequest) (Delivery, error) {
	return s.reconcile(ctx, "acknowledge", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.NativeAttemptRef)
}
func (s *Service) OutcomeUnknown(ctx context.Context, r ReconcileDeliveryRequest) (Delivery, error) {
	return s.reconcile(ctx, "outcome_unknown", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.NativeAttemptRef)
}
func (s *Service) FailDelivery(ctx context.Context, r FailDeliveryRequest) (Delivery, error) {
	if strings.TrimSpace(r.NativeAttemptRef) != "" {
		return s.reconcile(ctx, "fail", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.NativeAttemptRef)
	}
	return s.claimMutation(ctx, "fail_claimed", r.AdapterID, r.LeaseToken, r.OperationID, r.DeliveryID, r.ClaimToken, "", 0)
}

func (s *Service) claimMutation(ctx context.Context, kind, adapter, lease, op, delivery, token, native string, duration time.Duration) (Delivery, error) {
	if err := requireFields(adapter, lease, op, delivery, token); err != nil {
		return Delivery{}, err
	}
	if kind == "renew" {
		var err error
		duration, err = s.leaseDuration(duration)
		if err != nil {
			return Delivery{}, err
		}
	}
	fp := deliveryFingerprint(kind, delivery, native, duration.String())
	if result, found, err := s.replayDeliveryOperation(ctx, adapter, op, fp); err != nil || found {
		if err != nil {
			return Delivery{}, err
		}
		return result.Delivery, nil
	}
	result, err := s.store.DeliveryOperation(ctx, s.clock.Now().UTC(), store.DeliveryOperationRequest{Kind: kind, AdapterID: adapter, LeaseToken: lease, OperationID: op, Fingerprint: fp, DeliveryID: delivery, ClaimToken: token, NativeAttemptRef: native, ClaimDuration: duration})
	return result.Delivery, mapStoreError(err)
}
func (s *Service) reconcile(ctx context.Context, kind, adapter, lease, op, delivery, native string) (Delivery, error) {
	if err := requireFields(adapter, lease, op, delivery, native); err != nil {
		return Delivery{}, err
	}
	fp := deliveryFingerprint(kind, delivery, native)
	if result, found, err := s.replayDeliveryOperation(ctx, adapter, op, fp); err != nil || found {
		if err != nil {
			return Delivery{}, err
		}
		return result.Delivery, nil
	}
	result, err := s.store.DeliveryOperation(ctx, s.clock.Now().UTC(), store.DeliveryOperationRequest{Kind: kind, AdapterID: adapter, LeaseToken: lease, OperationID: op, Fingerprint: fp, DeliveryID: delivery, NativeAttemptRef: native})
	return result.Delivery, mapStoreError(err)
}
func (s *Service) replayDeliveryOperation(ctx context.Context, adapter, operation, fingerprint string) (store.DeliveryOperationResult, bool, error) {
	lookup, err := s.store.LookupDeliveryOperation(ctx, adapter, operation)
	if err != nil {
		return store.DeliveryOperationResult{}, false, mapStoreError(err)
	}
	if !lookup.Found {
		return store.DeliveryOperationResult{}, false, nil
	}
	if lookup.Fingerprint != fingerprint {
		return store.DeliveryOperationResult{}, false, &Error{Code: CodeOperationConflict, Err: store.ErrOperationConflict}
	}
	return lookup.Result, true, nil
}
func deliveryFingerprint(kind string, values ...any) string {
	b, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		Values []any  `json:"values"`
	}{kind, values})
	return fmt.Sprintf("%x", b)
}
func requireFields(values ...string) error {
	for _, v := range values {
		if err := required("field", v); err != nil {
			return err
		}
	}
	return nil
}
func invalid(message string) error { return &Error{Code: CodeInvalid, Err: errors.New(message)} }
